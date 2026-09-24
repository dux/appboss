package console

import (
	"testing"

	"dboss/internal/apps"
	"dboss/internal/config"
)

// A broken host file met while checking an app file must not mark a line in the app editor.
func TestYAMLLineSkipsHostFileErrors(t *testing.T) {
	err := &config.Error{Path: "/srv/dboss.local.yaml", Line: 3, Key: "management.url", Message: "was removed"}
	if line := yamlLine(err); line != 3 {
		t.Fatalf("own file error line = %d, want 3", line)
	}
	if line := yamlLine(&apps.HostFileError{Err: err}); line != 0 {
		t.Fatalf("host file error line = %d, want 0", line)
	}
}

func TestParseValuesKeepsEnvRefsLiteral(t *testing.T) {
	values, err := parseValues("postgres:\n  dsn: $DATABASE_URL\nproxy:\n  cloudflare_only: true\n")
	if err != nil {
		t.Fatal(err)
	}
	postgres, _ := values["postgres"].(map[string]any)
	if postgres["dsn"] != "$DATABASE_URL" {
		t.Errorf("dsn = %v, want the literal $VAR", postgres["dsn"])
	}
	proxy, _ := values["proxy"].(map[string]any)
	if proxy["cloudflare_only"] != true {
		t.Errorf("cloudflare_only = %#v", proxy["cloudflare_only"])
	}
}

func TestFilterRecipeValuesMovesBlanksToReset(t *testing.T) {
	allowed := map[string]bool{"pubsub.path": true, "pubsub.replay": true, "pubsub.secret": true}
	values, reset := filterRecipeValues(map[string]any{
		"pubsub.path":   "  ",
		"pubsub.replay": float64(5),
		"notify.url":    "ignored",
	}, []string{"pubsub.secret"}, allowed)
	if len(values) != 1 || values["pubsub.replay"] != float64(5) {
		t.Errorf("values = %#v", values)
	}
	if _, leaked := values["notify.url"]; leaked {
		t.Error("a key outside the recipe leaked through")
	}
	if len(reset) != 2 || reset[0] != "pubsub.secret" || reset[1] != "pubsub.path" {
		t.Errorf("reset = %v", reset)
	}
}
