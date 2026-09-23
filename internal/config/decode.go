package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// file is the full dboss.yaml schema: host keys plus app keys. Which role the file plays
// is decided after decoding from whether procfile or apps is present.
type file struct {
	Config  `yaml:",inline"`
	appFile `yaml:",inline"`
}

// hostKeys are the keys valid only in the root file. ParseApp rejects them in an app file.
var hostKeys = []string{"apps", "state_dir", "log_dir", "socket", "proxy", "management", "ports", "defaults", "daemon", "notify", "postgres"}

// hostTopKeys are the keys a root file may carry at its top level: the host-only keys plus the
// shared keys that are meaningful on the host itself. `hooks` is the one shared key that is a
// host-level block (the github_pr built-in), not only an app key under defaults:.
var hostTopKeys = append(append([]string{}, hostKeys...), "hooks")

// decode parses one document into raw and reports every top-level key present in it. The node
// tree is kept so every error can be pointed at a line and a key. allowDev lets the document be
// a dev session when it turns out to be an app; a file read as an app under a host never is.
func decode(data []byte, path string, raw *file, allowDev bool) (map[string]bool, *yaml.Node, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, located(err, path, nil)
	}
	if err := checkKeys(&root, reflect.TypeOf(file{}), ""); err != nil {
		return nil, nil, located(err, path, &root)
	}
	changed := applyDev(&root, allowDev && hasTopKey(&root, "procfile"))
	changed = expandEnv(&root, "") || changed
	if changed && len(root.Content) > 0 {
		if err := root.Content[0].Decode(raw); err != nil && !errors.Is(err, io.EOF) {
			return nil, nil, located(err, path, &root)
		}
	} else {
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(raw); err != nil && !errors.Is(err, io.EOF) {
			return nil, nil, located(err, path, &root)
		}
	}
	keys := map[string]bool{}
	if len(root.Content) > 0 && root.Content[0].Kind == yaml.MappingNode {
		for i := 0; i+1 < len(root.Content[0].Content); i += 2 {
			keys[root.Content[0].Content[i].Value] = true
		}
	}
	return keys, &root, nil
}

// envRef matches $NAME in a config value. Only all-uppercase names are eligible, so a bcrypt
// hash ($2a$10$...), a shell positional ($1) and lowercase shell vars are not touched.
var envRef = regexp.MustCompile(`\$[A-Z_][A-Z0-9_]*`)

// expandEnv replaces $NAME in every string value with the matching process environment
// variable, leaving the text as written when NAME is unset. procfile values and
// cron.*.command are runtime shell lines, so they are skipped. It reports whether any value
// changed, which tells decode to read the mutated tree instead of the original bytes.
func expandEnv(node *yaml.Node, path string) bool {
	changed := false
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			changed = expandEnv(child, path) || changed
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			child := path + key.Value + "."
			if strings.HasPrefix(child, "procfile.") || strings.HasPrefix(child, "cron.") && strings.HasSuffix(child, ".command.") {
				continue
			}
			changed = expandEnv(value, child) || changed
		}
	case yaml.ScalarNode:
		if node.Tag != "!!str" || !strings.Contains(node.Value, "$") {
			return false
		}
		value := envRef.ReplaceAllStringFunc(node.Value, func(match string) string {
			if env, ok := os.LookupEnv(match[1:]); ok {
				return env
			}
			return match
		})
		if value == node.Value {
			return false
		}
		node.Value, node.Tag, node.Style = value, "", 0
		return true
	}
	return changed
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(data, path)
}

// Parse builds the root config from data as if it had been read from path, so a document can be
// validated before it is written to disk. Relative paths resolve against path's directory.
func Parse(data []byte, path string) (Config, error) {
	raw := file{Config: Default()}
	keys, root, err := decode(data, path, &raw, true)
	if err != nil {
		return Config{}, err
	}
	cfg := raw.Config
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	cfg.SourcePath = absolutePath
	cfg.Dir = BaseDir(absolutePath)
	hasApp := keys["procfile"]
	if hasApp && keys["apps"] {
		return Config{}, located(&Error{Message: "a file is either an app (procfile) or a host (apps), not both", Hint: "move the host keys to the root dboss.yaml or drop apps"}, path, root)
	}
	if !hasApp {
		// A host file only accepts the host keys; the shared and app keys are ignored otherwise,
		// and silently ignoring a setting is worse than rejecting it.
		names := make([]string, 0, len(keys))
		for key := range keys {
			names = append(names, key)
		}
		sort.Strings(names)
		for _, key := range names {
			if !slices.Contains(hostTopKeys, key) {
				return Config{}, located(&Error{Key: key, Message: "is only valid in an app file", Hint: "put shared keys under defaults: in the host file"}, path, root)
			}
		}
	}
	if !hasApp {
		cfg.HostHooks = raw.Hooks
		if err := validateHostHooks(cfg.HostHooks); err != nil {
			return Config{}, located(err, path, root)
		}
	}
	cfg.Apps = resolvePath(cfg.Dir, cfg.Apps)
	if hasApp {
		// Single mode: the root file is the app, so the host apps directory does not apply.
		cfg.Apps = ""
	}
	cfg.StateDir = resolvePath(cfg.Dir, cfg.StateDir)
	cfg.LogDir = resolvePath(cfg.Dir, cfg.LogDir)
	cfg.Socket = resolvePath(cfg.Dir, cfg.Socket)
	cfg.Proxy.Wake.StartingPage = resolvePath(cfg.Dir, cfg.Proxy.Wake.StartingPage)
	cfg.Proxy.Wake.CrashedPage = resolvePath(cfg.Dir, cfg.Proxy.Wake.CrashedPage)
	cfg.Proxy.Wake.UnknownPage = resolvePath(cfg.Dir, cfg.Proxy.Wake.UnknownPage)
	if err := cfg.validate(hasApp); err != nil {
		return Config{}, located(err, path, root)
	}
	if hasApp {
		app, err := buildApp(raw.appFile, cfg.Defaults, true)
		if err != nil {
			return Config{}, located(err, path, root)
		}
		cfg.App = &app
	}
	return cfg, nil
}
