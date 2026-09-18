package config

import (
	"strings"
	"testing"
)

func TestKeysDocumentEveryConfigKey(t *testing.T) {
	keys := Keys()
	seen := map[string]Key{}
	for _, key := range keys {
		if key.Description == "" {
			t.Errorf("%s (%s) has no description", key.Path, key.Group)
		}
		if key.Default == "" && key.Example == "" {
			t.Errorf("%s (%s) has neither a default nor an example", key.Path, key.Group)
		}
		if _, dup := seen[key.Path]; dup {
			t.Errorf("%s is listed twice", key.Path)
		}
		seen[key.Path] = key
	}
	for _, path := range KeyPaths() {
		if _, ok := seen[path]; !ok {
			t.Errorf("keyDocs documents %s, which is not a config key any more", path)
		}
	}
	for path, want := range map[string]Key{
		"health":          {Group: GroupShared, Type: "string", Default: "tcp", PerProcess: true},
		"static":          {Group: GroupShared, Type: "string", Example: "./public"},
		"idle_stop":       {Group: GroupShared, Type: "duration", Default: "6h", PerProcess: true},
		"health_interval": {Group: GroupShared, Type: "duration", Default: "500ms", PerProcess: true},
		"restart_backoff": {Group: GroupShared, Type: "list", Default: "[1s, 2, 60s]", PerProcess: true},
		"ports.range":     {Group: GroupHost, Type: "[from, to]", Default: "[3100, 3990]"},
		"web_process":     {Group: GroupApp, Type: "string", Default: "web"},
		"autostart":       {Group: GroupApp, Type: "bool", Default: "true"},
		"management.host": {Group: GroupHost, Type: "list", Example: "boss.example.com"},
		"log_max_size":    {Group: GroupShared, Type: "size", Default: "10m", PerProcess: true},
	} {
		got, ok := seen[path]
		if !ok {
			t.Errorf("%s is missing", path)
			continue
		}
		if got.Group != want.Group || got.Type != want.Type || got.Default != want.Default || got.Example != want.Example || got.PerProcess != want.PerProcess {
			t.Errorf("%s = %+v, want group=%s type=%s default=%q example=%q per_process=%v", path, got, want.Group, want.Type, want.Default, want.Example, want.PerProcess)
		}
	}
	if !strings.HasPrefix(keys[0].Path, "apps") || keys[len(keys)-1].Group != GroupShared {
		t.Errorf("unexpected order: first %s, last group %s", keys[0].Path, keys[len(keys)-1].Group)
	}
}
