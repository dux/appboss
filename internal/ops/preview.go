package ops

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"dboss/internal/config"
	"dboss/internal/super"
)

var (
	identDisallowed = regexp.MustCompile(`[^a-z0-9_]`)
	hostDisallowed  = regexp.MustCompile(`[^a-z0-9.*-]`)
)

// RunHostHook runs a host-level hook. Only the github_pr built-in exists: it deploys or tears
// down one preview app from the request params.
func (s *Service) RunHostHook(name string, params map[string]string) error {
	if name != config.BuiltinGithubPR {
		return fmt.Errorf("unknown host hook %q", name)
	}
	return s.runGithubPR(name, params)
}

// previewCtx decides how a value interpolated from the request is sanitized: a host is DNS-safe
// (dashes, dots and a wildcard), everything else is an identifier (underscores only).
type previewCtx int

const (
	ctxIdentifier previewCtx = iota
	ctxHost
)

func (s *Service) runGithubPR(hookName string, params map[string]string) error {
	cfg := s.runtime.HostConfig()
	hook, ok := cfg.HostHooks[hookName]
	if !ok {
		return fmt.Errorf("unknown host hook %q", hookName)
	}
	action := strings.ToLower(strings.TrimSpace(params["QS_ACTION"]))
	branch := strings.TrimSpace(params["QS_BRANCH"])
	if branch == "" {
		return errors.New("branch is required")
	}
	repo := strings.TrimSpace(params["QS_REPO"])
	if repo == "" {
		repo = hook.Repo
	}
	if repo == "" {
		return errors.New("repo is required")
	}
	num := strings.TrimSpace(params["QS_NUM"])

	vars := map[string]string{"QS_ACTION": action, "QS_BRANCH": branch, "QS_REPO": repo, "QS_NUM": num}
	nameTemplate, _ := hook.Template["name"].(string)
	appName := sanitize(expand(nameTemplate, vars, "name"))
	if appName == "" {
		return errors.New("the template name expands to nothing")
	}
	switch appName {
	case "main", "development":
		return fmt.Errorf("refusing to deploy reserved name %q", appName)
	}

	lock := s.previewLock(appName)
	lock.Lock()
	defer lock.Unlock()

	actor := "hook:" + hookName
	detail := fmt.Sprintf("branch=%s num=%s", branch, num)
	if action == "closed" {
		return s.teardownPreview(cfg, appName, actor, detail)
	}
	return s.deployPreview(cfg, hook, appName, repo, branch, actor, detail, vars)
}

func (s *Service) deployPreview(cfg config.Config, hook config.Hook, appName, repo, branch, actor, detail string, vars map[string]string) error {
	appDir := filepath.Join(cfg.Apps, appName)
	// Stop a running preview before resetting its checkout, so git never touches files an Odoo
	// process is reading. An unknown app (first deploy) is fine to ignore.
	_ = s.runtime.Stop(appName)
	if err := s.checkout(appDir, repo, branch, cfg); err != nil {
		s.Audit(actor, appName, "deploy", detail, err)
		return err
	}

	rendered, err := renderTemplate(hook.Template, vars)
	if err != nil {
		s.Audit(actor, appName, "config-write", detail, err)
		return err
	}
	configPath := filepath.Join(appDir, config.FileName)
	// Validate before writing, so a bad template is a clear error rather than a broken app on rescan.
	if _, err := config.ParseApp(rendered, configPath, cfg.Defaults); err != nil {
		s.Audit(actor, appName, "config-write", detail, err)
		return err
	}
	if err := os.WriteFile(configPath, rendered, 0o644); err != nil {
		s.Audit(actor, appName, "config-write", detail, err)
		return err
	}
	s.Audit(actor, appName, "config-write", detail, nil)

	if _, err := s.runtime.Rescan(); err != nil {
		s.Audit(actor, appName, "deploy", detail, err)
		return err
	}
	if _, err := s.runtime.Snapshot(appName); err != nil {
		s.Audit(actor, appName, "deploy", detail, err)
		return fmt.Errorf("app %s did not load after rescan: %w", appName, err)
	}

	if err := s.runtime.Start(appName); err != nil {
		s.Audit(actor, appName, "deploy", detail, err)
		return err
	}
	s.Audit(actor, appName, "deploy", detail, nil)
	return nil
}

func (s *Service) teardownPreview(cfg config.Config, appName, actor, detail string) error {
	appDir := filepath.Join(cfg.Apps, appName)
	if _, err := os.Stat(appDir); err != nil {
		return nil
	}
	_ = s.runtime.Stop(appName)
	s.Audit(actor, appName, "stop", detail, nil)
	if err := s.runtime.Destroy(appName); err != nil {
		s.Audit(actor, appName, "destroy", detail, err)
		return err
	}
	s.Audit(actor, appName, "destroy", detail, nil)
	return nil
}

// checkout clones the branch on first deploy and resets to it afterwards, so a repeat ping
// lands on the same checkout. The remote is re-pointed every time, since a fork PR changes it.
func (s *Service) checkout(appDir, repo, branch string, cfg config.Config) error {
	env := os.Environ()
	if token := cfg.Defaults.GithubToken; token != "" {
		extra := map[string]string{}
		super.GitAuthEnv(extra, token)
		for key, value := range extra {
			env = append(env, key+"="+value)
		}
	}
	git := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Env = env
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
		}
		return nil
	}
	if _, err := os.Stat(filepath.Join(appDir, ".git")); err == nil {
		if err := git("-C", appDir, "remote", "set-url", "origin", repo); err != nil {
			return err
		}
		if err := git("-C", appDir, "fetch", "--prune", "origin"); err != nil {
			return err
		}
		return git("-C", appDir, "reset", "--hard", "origin/"+branch)
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return err
	}
	return git("clone", "--branch", branch, repo, appDir)
}

// renderTemplate expands every string in the template and drops the preview-only `name` key,
// leaving a valid app file. A top-level `hosts` is the default for every procfile entry that
// declares none, so one host covers the usual single web process.
func renderTemplate(template map[string]any, vars map[string]string) ([]byte, error) {
	body := make(map[string]any, len(template))
	var topHosts any
	for key, value := range template {
		switch key {
		case "name":
			continue
		case "hosts":
			topHosts = interpolateValue(value, vars, "hosts")
			continue
		}
		body[key] = interpolateValue(value, vars, key)
	}
	if topHosts != nil {
		procfile, ok := body["procfile"].(map[string]any)
		if !ok {
			return nil, errors.New("template sets hosts but has no procfile mapping")
		}
		for _, entry := range procfile {
			fields, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if _, has := fields["hosts"]; !has {
				fields["hosts"] = topHosts
			}
		}
	}
	return yaml.Marshal(body)
}

func interpolateValue(value any, vars map[string]string, key string) any {
	switch v := value.(type) {
	case string:
		return expand(v, vars, key)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = interpolateValue(item, vars, key)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = interpolateValue(item, vars, k)
		}
		return out
	default:
		return value
	}
}

// expand replaces $NAME and ${NAME} with the request value sanitized for the interpolation
// context. Literal text is never touched; a host value is cleaned as a whole afterwards.
func expand(s string, vars map[string]string, key string) string {
	if s == "" || !strings.ContainsRune(s, '$') {
		return s
	}
	ctx := ctxIdentifier
	if key == "hosts" || key == "canonical_host" {
		ctx = ctxHost
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			if end := strings.IndexByte(s[i+2:], '}'); end >= 0 {
				if value, ok := vars[s[i+2:i+2+end]]; ok {
					b.WriteString(sanitizeValue(value, ctx))
					i += 2 + end + 1
					continue
				}
			}
			b.WriteByte('$')
			i++
			continue
		}
		j := i + 1
		for j < len(s) && isVarByte(s[j]) {
			j++
		}
		if j > i+1 {
			if value, ok := vars[s[i+1:j]]; ok {
				b.WriteString(sanitizeValue(value, ctx))
				i = j
				continue
			}
		}
		b.WriteByte('$')
		i++
	}
	if ctx == ctxHost {
		return cleanHost(b.String())
	}
	return b.String()
}

func isVarByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z')
}

// sanitizeValue reduces a request value to what the context allows: identifiers keep lower-case
// letters, digits and underscore, a host keeps a DNS label (dash, dot, wildcard).
func sanitizeValue(value string, ctx previewCtx) string {
	value = strings.ToLower(value)
	if ctx == ctxHost {
		value = strings.NewReplacer("/", "-", "_", "-").Replace(value)
		return hostDisallowed.ReplaceAllString(value, "")
	}
	value = strings.NewReplacer("/", "_", "-", "_").Replace(value)
	return identDisallowed.ReplaceAllString(value, "")
}

// cleanHost finishes a completed host string: any underscore becomes a dash and anything a DNS
// label cannot hold is dropped.
func cleanHost(value string) string {
	value = strings.ToLower(strings.ReplaceAll(value, "_", "-"))
	return hostDisallowed.ReplaceAllString(value, "")
}

// sanitize keeps an identifier (an app name, a database name) to lower-case letters, digits and
// underscores.
func sanitize(value string) string {
	value = strings.ToLower(value)
	value = strings.NewReplacer("/", "_", "-", "_").Replace(value)
	return identDisallowed.ReplaceAllString(value, "")
}
