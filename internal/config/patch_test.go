package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func decodeYAML(t *testing.T, contents string) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := yaml.Unmarshal([]byte(contents), &out); err != nil {
		t.Fatalf("output is not YAML: %v\n%s", err, contents)
	}
	return out
}

func TestPatchYAMLSetsNestedPaths(t *testing.T) {
	base := "apps: ./apps\n\nproxy:\n  listen: \":80\"\n\n# keep me\npubsub:\n  path: /old\n"
	out, err := PatchYAML(base, map[string]any{"pubsub.path": "/new", "notify.url": "https://x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# keep me") {
		t.Errorf("comment was dropped:\n%s", out)
	}
	values := decodeYAML(t, out)
	if values["apps"] != "./apps" {
		t.Errorf("apps changed to %v", values["apps"])
	}
	pubsub, _ := values["pubsub"].(map[string]any)
	if pubsub["path"] != "/new" {
		t.Errorf("pubsub.path = %v", pubsub["path"])
	}
	notify, _ := values["notify"].(map[string]any)
	if notify["url"] != "https://x" {
		t.Errorf("notify.url = %v", notify["url"])
	}
}

func TestPatchYAMLResetPrunesEmptyParents(t *testing.T) {
	out, err := PatchYAML("apps: ./apps\npubsub:\n  path: /x\n", nil, []string{"pubsub.path"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "pubsub") {
		t.Errorf("empty pubsub mapping was not pruned:\n%s", out)
	}
}

func TestPatchYAMLResetKeepsPopulatedParents(t *testing.T) {
	out, err := PatchYAML("apps: ./apps\npubsub:\n  path: /x\n  replay: 10\n", nil, []string{"pubsub.path"})
	if err != nil {
		t.Fatal(err)
	}
	values := decodeYAML(t, out)
	pubsub, _ := values["pubsub"].(map[string]any)
	if _, stale := pubsub["path"]; stale {
		t.Errorf("pubsub.path was not removed: %v", pubsub)
	}
	if pubsub["replay"] != 10 {
		t.Errorf("pubsub.replay = %#v", pubsub["replay"])
	}
}

func TestPatchYAMLWritesTypedValues(t *testing.T) {
	out, err := PatchYAML("apps: ./apps\n", map[string]any{
		"notify.events":  []any{"crash", "deploy"},
		"notify.headers": map[string]any{"Authorization": "Bearer x"},
		"postgres.dsn":   "postgres://app",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	values := decodeYAML(t, out)
	if values["postgres"].(map[string]any)["dsn"] != "postgres://app" {
		t.Errorf("postgres.dsn = %v", values["postgres"])
	}
	events, ok := values["notify"].(map[string]any)["events"].([]any)
	if !ok || len(events) != 2 || events[0] != "crash" {
		t.Errorf("notify.events = %#v", values["notify"])
	}
	headers, ok := values["notify"].(map[string]any)["headers"].(map[string]any)
	if !ok || headers["Authorization"] != "Bearer x" {
		t.Errorf("notify.headers = %#v", values["notify"])
	}
}

func TestPatchYAMLRejectsScalarParent(t *testing.T) {
	if _, err := PatchYAML("apps: ./apps\npostgres: oops\n", map[string]any{"postgres.dsn": "x"}, nil); err == nil {
		t.Fatal("expected an error when the parent is not a mapping")
	}
}
