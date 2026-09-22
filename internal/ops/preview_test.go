package ops

import (
	"testing"

	"gopkg.in/yaml.v3"

	"dboss/internal/config"
)

func TestExpandContexts(t *testing.T) {
	vars := map[string]string{"QS_BRANCH": "Feature/x"}
	cases := map[string]struct {
		key  string
		want string
	}{
		"identifier":   {key: "name", want: "feature_x"},
		"braces":       {key: "name", want: "feature_x_erpx"},
		"host slashes": {key: "hosts", want: "pr-feature-x.lvh.me"},
	}
	for name, want := range cases {
		var input string
		switch name {
		case "identifier":
			input = "$QS_BRANCH"
		case "braces":
			input = "${QS_BRANCH}_erpx"
		case "host slashes":
			input = "pr-$QS_BRANCH.lvh.me"
		}
		if got := expand(input, vars, want.key); got != want.want {
			t.Errorf("%s: expand(%q) = %q, want %q", name, input, got, want.want)
		}
	}
}

func TestExpandHostUnderscoreBecomesDash(t *testing.T) {
	vars := map[string]string{"QS_BRANCH": "feature_x"}
	if got := expand("pr-$QS_BRANCH.example.com", vars, "hosts"); got != "pr-feature-x.example.com" {
		t.Fatalf("host = %q", got)
	}
}

func TestRenderTemplateAppliesTopHosts(t *testing.T) {
	template := map[string]any{
		"name":      "$QS_BRANCH",
		"hosts":     []any{"pr-$QS_BRANCH.lvh.me"},
		"autostart": false,
		"procfile": map[string]any{
			"web": map[string]any{"command": "./start.sh"},
		},
	}
	out, err := renderTemplate(template, map[string]string{"QS_BRANCH": "feature/x"})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["name"]; ok {
		t.Fatalf("rendered template leaked the name key: %s", out)
	}
	app, err := config.ParseApp(out, "/tmp/dboss.yaml", config.Default().Defaults)
	if err != nil {
		t.Fatalf("rendered app is invalid: %v\n%s", err, out)
	}
	hosts := []string(app.Hosts)
	if len(hosts) != 1 || hosts[0] != "pr-feature-x.lvh.me" {
		t.Fatalf("hosts = %v", hosts)
	}
}
