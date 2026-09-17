package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMergesDefaultsAndRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy-boss.config.yaml")
	if err := os.WriteFile(path, []byte("apps: [./apps/demo]\ndefaults:\n  idle_stop: 2h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Apps[0] != filepath.Join(dir, "apps", "demo") || cfg.Defaults.IdleStop.Value() != 2*time.Hour || cfg.Ports.Range[0] != 3100 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if err := os.WriteFile(path, []byte("unknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown-key error")
	}
}

func TestLoadAppRequiresProcfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy-boss.yaml")
	if err := os.WriteFile(path, []byte("hosts: [demo.test]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApp(path, Default().Defaults); err == nil {
		t.Fatal("expected missing procfile error")
	}
	if err := os.WriteFile(path, []byte("procfile:\n  web: ./server\nhosts: [demo.test]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := LoadApp(path, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.Procfile["web"] != "./server" {
		t.Fatalf("unexpected procfile: %#v", app.Procfile)
	}
}

func TestParseSize(t *testing.T) {
	got, err := ParseSize("512m")
	if err != nil || got != 512<<20 {
		t.Fatalf("got %d, %v", got, err)
	}
}
