package config

import (
	"strings"
	"testing"
)

func TestKeysDocumentEveryConfigKey(t *testing.T) {
	keys := Keys()
	seen := map[string]Key{}
	for _, key := range keys {
		if key.Block == "" || key.Name == "" || key.Description == "" {
			t.Errorf("%s (%s) is missing block, name or description", key.Path, key.Block)
		}
		if key.Default == "" && key.Example == "" {
			t.Errorf("%s (%s) has neither a default nor an example", key.Path, key.Block)
		}
		if _, dup := seen[key.Path]; dup {
			t.Errorf("%s is listed twice", key.Path)
		}
		seen[key.Path] = key
	}
	for _, path := range KeyPaths() {
		if _, ok := seen[path]; !ok {
			t.Errorf("keySpecs documents %s, which is not a config key any more", path)
		}
	}
	for path, want := range map[string]Key{
		"static":          {Block: "web", Scope: ScopeBoth, Type: "string", Default: "./public", Example: "./dist"},
		"idle_stop":       {Block: "runtime", Scope: ScopeBoth, Type: "duration", Default: "6h", Example: "30m", PerProcess: true},
		"health_interval": {Block: "runtime", Scope: ScopeBoth, Type: "duration", Default: "500ms", Example: "1s", PerProcess: true},
		"restart_backoff": {Block: "runtime", Scope: ScopeBoth, Type: "list", Default: "[1s, 2, 60s]", Example: "[500ms, 2.0, 30s]", PerProcess: true},
		"ports.range":     {Block: "ports", Scope: ScopeService, Type: "[from, to]", Default: "[3100, 3990]"},
		"autostart":       {Block: "app", Scope: ScopeApp, Type: "bool | button", Default: "true"},
		"deletable":       {Block: "app", Scope: ScopeApp, Type: "bool", Default: "false"},
		"management.host": {Block: "management", Scope: ScopeService, Type: "string | list", Example: "dboss.example.com"},
		"log_max_size":    {Block: "runtime", Scope: ScopeBoth, Type: "size", Default: "10m", Example: "50m", PerProcess: true},
	} {
		got, ok := seen[path]
		if !ok {
			t.Errorf("%s is missing", path)
			continue
		}
		if got.Block != want.Block || got.Scope != want.Scope || got.Type != want.Type || got.Default != want.Default || got.Example != want.Example || got.PerProcess != want.PerProcess {
			t.Errorf("%s = %+v, want block=%s scope=%s type=%s default=%q example=%q per_process=%v", path, got, want.Block, want.Scope, want.Type, want.Default, want.Example, want.PerProcess)
		}
	}
	if !strings.HasPrefix(keys[0].Path, "apps") {
		t.Errorf("unexpected order: first %s", keys[0].Path)
	}
}

func TestKeySpecsUseRealBlocks(t *testing.T) {
	known := map[string]Block{}
	for _, block := range Blocks() {
		if block.ID == "" || block.Title == "" || block.Summary == "" {
			t.Errorf("block %q is missing id, title or summary", block.ID)
		}
		if block.Scope != ScopeService && block.Scope != ScopeApp && block.Scope != ScopeBoth {
			t.Errorf("block %q has invalid scope %q", block.ID, block.Scope)
		}
		if _, dup := known[block.ID]; dup {
			t.Errorf("block %q is listed twice", block.ID)
		}
		known[block.ID] = block
	}
	counts := map[string]int{}
	for path, spec := range keySpecs {
		if _, ok := known[spec.Block]; !ok {
			t.Errorf("%s names unknown block %q", path, spec.Block)
			continue
		}
		counts[spec.Block]++
	}
	for id := range known {
		if counts[id] == 0 {
			t.Errorf("block %s has no keys", id)
		}
	}
}
