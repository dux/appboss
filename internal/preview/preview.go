// Package preview turns a github_pr hook ping into a preview app: which app, from which branch,
// and the app file rendered from the host's template. It only computes; ops runs the deploy.
package preview

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"dboss/internal/config"
)

var (
	identDisallowed = regexp.MustCompile(`[^a-z0-9_]`)
	hostDisallowed  = regexp.MustCompile(`[^a-z0-9.*-]`)
)

// Request is one ping resolved against the hook: the preview app it targets and the values the
// template expands.
type Request struct {
	App    string
	Close  bool
	Repo   string
	Branch string
	Num    string
	Vars   map[string]string
}

// Parse reads a ping's QS_ params. The template's name expands to the app name, which must not
// be empty or one of the long-lived branches.
func Parse(hook config.Hook, params map[string]string) (Request, error) {
	action := strings.ToLower(strings.TrimSpace(params["QS_ACTION"]))
	branch := strings.TrimSpace(params["QS_BRANCH"])
	if branch == "" {
		return Request{}, errors.New("branch is required")
	}
	repo := strings.TrimSpace(params["QS_REPO"])
	if repo == "" {
		repo = hook.Repo
	}
	if repo == "" {
		return Request{}, errors.New("repo is required")
	}
	num := strings.TrimSpace(params["QS_NUM"])
	vars := map[string]string{"QS_ACTION": action, "QS_BRANCH": branch, "QS_REPO": repo, "QS_NUM": num}
	nameTemplate, _ := hook.Template["name"].(string)
	// The whole name, template text included, becomes an identifier: it is the app's folder.
	app := sanitizeValue(expand(nameTemplate, vars, "name"), ctxIdentifier)
	if app == "" {
		return Request{}, errors.New("the template name expands to nothing")
	}
	switch app {
	case "main", "development":
		return Request{}, fmt.Errorf("refusing to deploy reserved name %q", app)
	}
	return Request{App: app, Close: action == "closed", Repo: repo, Branch: branch, Num: num, Vars: vars}, nil
}

// context decides how a value interpolated from the request is sanitized: a host is DNS-safe
// (dashes, dots and a wildcard), everything else is an identifier (underscores only).
type context int

const (
	ctxIdentifier context = iota
	ctxHost
)

// Render expands every string in the template and drops the preview-only `name` key,
// leaving a valid app file. A top-level `hosts` is the default for every procfile entry that
// declares none, so one host covers the usual single web process.
func Render(template map[string]any, vars map[string]string) ([]byte, error) {
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
func sanitizeValue(value string, ctx context) string {
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
