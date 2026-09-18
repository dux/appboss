package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadHostMergesDefaultsAndRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	writeConfigFile(t, path, "apps: ./apps\ndefaults:\n  idle_stop: 2h\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Apps != filepath.Join(dir, "apps") || cfg.Defaults.IdleStop.Value() != 2*time.Hour || cfg.Ports.Range[0] != 3100 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.Dir != dir || cfg.App != nil || cfg.Socket != filepath.Join(dir, ".dboss", "dboss.sock") {
		t.Fatalf("unexpected root fields: dir=%q app=%v socket=%q", cfg.Dir, cfg.App, cfg.Socket)
	}
	writeConfigFile(t, path, "apps: ./apps\nunknown: true\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown-key error")
	}
}

func TestLoadSingleAppRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	writeConfigFile(t, path, "procfile:\n  web: ./server\nhosts: [demo.test]\nidle_stop: 0s\nproxy:\n  listen: 127.0.0.1:9090\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.App == nil || cfg.App.Procfile["web"] != "./server" || cfg.App.IdleStop != 0 || cfg.Proxy.Listen != "127.0.0.1:9090" {
		t.Fatalf("unexpected single-app config: %+v app=%+v", cfg, cfg.App)
	}
	if cfg.Apps != "" {
		t.Fatalf("apps should be empty in single mode, got %q", cfg.Apps)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsAmbiguousOrEmptyRole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	writeConfigFile(t, path, "procfile:\n  web: ./server\napps: ./apps\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("expected both-set error, got %v", err)
	}
	writeConfigFile(t, path, "defaults:\n  idle_stop: 1h\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected missing role error")
	}
}

func TestFindInDirPrefersLocalFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := FindInDir(dir); err == nil {
		t.Fatal("expected missing config error")
	}
	writeConfigFile(t, filepath.Join(dir, FileName), "procfile:\n  web: ./server\n")
	if path, err := FindInDir(dir); err != nil || path != filepath.Join(dir, FileName) {
		t.Fatalf("got %q, %v", path, err)
	}
	writeConfigFile(t, filepath.Join(dir, LocalFileName), "procfile:\n  web: ./local-server\n")
	if path, err := FindInDir(dir); err != nil || path != filepath.Join(dir, LocalFileName) {
		t.Fatalf("got %q, %v", path, err)
	}
}

func TestLoadAppRequiresProcfileAndRejectsHostKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	writeConfigFile(t, path, "hosts: [demo.test]\n")
	if _, err := LoadApp(path, Default().Defaults); err == nil {
		t.Fatal("expected missing procfile error")
	}
	writeConfigFile(t, path, "procfile:\n  web: ./server\nhosts: [demo.test]\n")
	app, err := LoadApp(path, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.Procfile["web"] != "./server" {
		t.Fatalf("unexpected procfile: %#v", app.Procfile)
	}
	writeConfigFile(t, path, "procfile:\n  web: ./server\nproxy:\n  listen: 127.0.0.1:9090\n")
	if _, err := LoadApp(path, Default().Defaults); err == nil || !strings.Contains(err.Error(), "proxy is only valid in the root") {
		t.Fatalf("expected host-key error, got %v", err)
	}
}

func TestManagementRequiresAuthAndProxyListener(t *testing.T) {
	cfg := Default()
	cfg.Apps = "/apps"
	cfg.Management.Host = "boss.example.com"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing admin email error")
	}
	cfg.Management.Auth.AdminEmails = []string{"admin@example.com"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Proxy.Listen = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing proxy listener error")
	}
}

func TestTrustedCIDRsMustParse(t *testing.T) {
	cfg := Default()
	cfg.Apps = "/apps"
	cfg.Proxy.TrustedCIDRs = []string{"173.245.48.0/20", "2400:cb00::/32"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Proxy.TrustedCIDRs = []string{"173.245.48.0"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid cidr error")
	}
}

func TestParseSize(t *testing.T) {
	got, err := ParseSize("512m")
	if err != nil || got != 512<<20 {
		t.Fatalf("got %d, %v", got, err)
	}
}
