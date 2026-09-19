package console

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

func TestApplyFormPatchSetsNestedPaths(t *testing.T) {
	base := "apps: ./apps\n\nproxy:\n  listen: \":80\"\n\n# keep me\npubsub:\n  path: /old\n"
	out, err := applyFormPatch(base, map[string]any{"pubsub.path": "/new", "s3.endpoint": "https://x"}, nil)
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
	s3, _ := values["s3"].(map[string]any)
	if s3["endpoint"] != "https://x" {
		t.Errorf("s3.endpoint = %v", s3["endpoint"])
	}
}

func TestApplyFormPatchResetPrunesEmptyParents(t *testing.T) {
	out, err := applyFormPatch("apps: ./apps\npubsub:\n  path: /x\n", nil, []string{"pubsub.path"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "pubsub") {
		t.Errorf("empty pubsub mapping was not pruned:\n%s", out)
	}
}

func TestApplyFormPatchResetKeepsPopulatedParents(t *testing.T) {
	out, err := applyFormPatch("apps: ./apps\npubsub:\n  path: /x\n  replay: 10\n", nil, []string{"pubsub.path"})
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

func TestApplyFormPatchWritesTypedValues(t *testing.T) {
	out, err := applyFormPatch("apps: ./apps\n", map[string]any{
		"notify.events":  []any{"crash", "deploy"},
		"notify.headers": map[string]any{"Authorization": "Bearer x"},
		"s3.bucket":      "backups",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	values := decodeYAML(t, out)
	if values["s3"].(map[string]any)["bucket"] != "backups" {
		t.Errorf("s3.bucket = %v", values["s3"])
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

func TestApplyFormPatchRejectsScalarParent(t *testing.T) {
	if _, err := applyFormPatch("apps: ./apps\ns3: oops\n", map[string]any{"s3.endpoint": "x"}, nil); err == nil {
		t.Fatal("expected an error when the parent is not a mapping")
	}
}

func TestParseValuesKeepsEnvRefsLiteral(t *testing.T) {
	values, err := parseValues("s3:\n  access_key: $S3_ACCESS_KEY\n  path_style: true\n")
	if err != nil {
		t.Fatal(err)
	}
	s3, _ := values["s3"].(map[string]any)
	if s3["access_key"] != "$S3_ACCESS_KEY" {
		t.Errorf("access_key = %v, want the literal $VAR", s3["access_key"])
	}
	if s3["path_style"] != true {
		t.Errorf("path_style = %#v", s3["path_style"])
	}
}

func TestFilterRecipeValuesMovesBlanksToReset(t *testing.T) {
	allowed := map[string]bool{"pubsub.path": true, "pubsub.replay": true, "pubsub.secret": true}
	values, reset := filterRecipeValues(map[string]any{
		"pubsub.path":   "  ",
		"pubsub.replay": float64(5),
		"s3.endpoint":   "ignored",
	}, []string{"pubsub.secret"}, allowed)
	if len(values) != 1 || values["pubsub.replay"] != float64(5) {
		t.Errorf("values = %#v", values)
	}
	if _, leaked := values["s3.endpoint"]; leaked {
		t.Error("a key outside the recipe leaked through")
	}
	if len(reset) != 2 || reset[0] != "pubsub.secret" || reset[1] != "pubsub.path" {
		t.Errorf("reset = %v", reset)
	}
}
