package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTemplateCoversKeys(t *testing.T) {
	for _, role := range []string{TemplateService, TemplateApp} {
		lines, err := initLines(role)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		got := map[string]bool{}
		for _, line := range lines {
			if line.kind == kindYAML && line.key != "" {
				if got[line.key] {
					t.Errorf("%s: %s listed twice", role, line.key)
				}
				got[line.key] = true
			}
		}
		want := map[string]bool{}
		for _, key := range Keys() {
			switch role {
			case TemplateService:
				if key.Scope == ScopeService {
					want[key.Path] = true
				}
				if key.Scope == ScopeBoth {
					want["defaults."+key.Path] = true
				}
			case TemplateApp:
				if key.Scope == ScopeApp || key.Scope == ScopeBoth {
					want[key.Path] = true
				}
			}
		}
		for path := range want {
			if !got[path] {
				t.Errorf("%s: missing %s", role, path)
			}
		}
		for path := range got {
			if !want[path] {
				t.Errorf("%s: unexpected %s", role, path)
			}
		}
	}
}

func TestTemplateIsCommented(t *testing.T) {
	for _, role := range []string{TemplateService, TemplateApp} {
		out, err := Template(role)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if lines[len(lines)-1] != "# dboss init > dboss.yaml to save config" {
			t.Errorf("%s: last line is %q", role, lines[len(lines)-1])
		}
		for _, line := range lines {
			if line != "" && !strings.HasPrefix(line, "#") {
				t.Errorf("%s: line is not commented: %q", role, line)
			}
		}
	}
}

// TestTemplateYAMLParses uncomments the template's YAML lines and decodes them, so a value the
// generator writes for a default or an example must be valid YAML on its own.
func TestTemplateYAMLParses(t *testing.T) {
	for _, role := range []string{TemplateService, TemplateApp} {
		lines, err := initLines(role)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		var b strings.Builder
		for _, line := range lines {
			if line.kind == kindYAML {
				b.WriteString(strings.Repeat("  ", line.indent) + line.text + "\n")
			}
		}
		var document any
		if err := yaml.Unmarshal([]byte(b.String()), &document); err != nil {
			t.Errorf("%s: template is not valid YAML: %v", role, err)
		}
	}
}

func TestTemplateRoles(t *testing.T) {
	service, err := Template(TemplateService)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(service, "# proxy:") || !strings.Contains(service, "# defaults:") || strings.Contains(service, "# procfile:") {
		t.Error("service template does not look like a root config")
	}
	app, err := Template(TemplateApp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(app, "# procfile:") || !strings.Contains(app, "# idle_stop:") || strings.Contains(app, "# proxy:") {
		t.Error("app template does not look like an app config")
	}
	if _, err := Template("nope"); err == nil {
		t.Error("unknown role should fail")
	}
}
