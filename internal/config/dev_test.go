package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// devFile is an app document, so Parse reads it as a dev session.
const devFile = `procfile:
  web:
    command: ./server -e production
    command_dev: ./server -e development
    hosts: [demo.test]
    hosts_dev: [demo.lvh.me]
idle_stop: 6h
idle_stop_dev: 0s
proxy:
  listen: ":80"
  listen_dev: ":3000"
env:
  API_URL: https://api.example.com
  API_URL_dev: http://lvh.me:4000
`

func TestDevSuffixOverridesInSingleAppMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	writeConfigFile(t, path, devFile)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Dev() {
		t.Fatal("an app file loaded as the root config is a dev session")
	}
	if got := strings.Join(cfg.Proxy.Listen, ","); got != ":3000" {
		t.Fatalf("proxy.listen = %q, want :3000", got)
	}
	web := cfg.App.Procfile["web"]
	if web.Command != "./server -e development" {
		t.Fatalf("procfile.web.command = %q", web.Command)
	}
	if got := strings.Join(web.Hosts, ","); got != "demo.lvh.me" {
		t.Fatalf("procfile.web.hosts = %q", got)
	}
	if cfg.App.IdleStop != 0 {
		t.Fatalf("idle_stop = %s, want 0s", cfg.App.IdleStop.Value())
	}
	if got := cfg.App.Env["API_URL"]; got != "http://lvh.me:4000" {
		t.Fatalf("env.API_URL = %q", got)
	}
	if _, ok := cfg.App.Env["API_URL_dev"]; ok {
		t.Fatal("the _dev entry must not survive as its own env var")
	}
}

// The same file under a host is not a dev session, so every _dev value is dropped and the base
// value stands. This is what lets one committed app file serve both.
func TestDevSuffixDroppedUnderHost(t *testing.T) {
	app, err := ParseApp([]byte(strings.ReplaceAll(devFile, "proxy:\n  listen: \":80\"\n  listen_dev: \":3000\"\n", "")), "/srv/apps/demo/"+FileName, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	web := app.Procfile["web"]
	if web.Command != "./server -e production" {
		t.Fatalf("procfile.web.command = %q", web.Command)
	}
	if got := strings.Join(web.Hosts, ","); got != "demo.test" {
		t.Fatalf("procfile.web.hosts = %q", got)
	}
	if app.IdleStop.Value() != 6*time.Hour {
		t.Fatalf("idle_stop = %s, want 6h", app.IdleStop.Value())
	}
	if got := app.Env["API_URL"]; got != "https://api.example.com" {
		t.Fatalf("env.API_URL = %q", got)
	}
}

// A host file is never dev, so its _dev keys are dropped rather than applied.
func TestDevSuffixDroppedInHostFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	writeConfigFile(t, path, "apps: ./apps\nproxy:\n  listen: \":80\"\n  listen_dev: \":3000\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dev() {
		t.Fatal("a host file is not a dev session")
	}
	if got := strings.Join(cfg.Proxy.Listen, ","); got != ":80" {
		t.Fatalf("proxy.listen = %q, want :80", got)
	}
}

// A _dev key with no base key sets the value in dev and is absent otherwise.
func TestDevSuffixWithoutBaseKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	document := "procfile:\n  web:\n    command: ./server\n    hosts: [demo.test]\nheaders_dev:\n  X-Robots-Tag: noindex\n"
	writeConfigFile(t, path, document)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.App.Headers["X-Robots-Tag"] != "noindex" {
		t.Fatalf("headers = %v", cfg.App.Headers)
	}
	app, err := ParseApp([]byte(document), "/srv/apps/demo/"+FileName, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Headers) != 0 {
		t.Fatalf("headers should be empty under a host, got %v", app.Headers)
	}
}

// The suffix does not weaken the schema check: the base key still has to exist, in both modes.
func TestDevSuffixStillRejectsUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	for _, document := range []string{
		"procfile:\n  web: ./server\nbogus_dev: 1\n",
		"apps: ./apps\nbogus_dev: 1\n",
		"procfile:\n  web: ./server\nproxy:\n  bogus_dev: 1\n",
	} {
		writeConfigFile(t, path, document)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown key") {
			t.Fatalf("%q: expected unknown-key error, got %v", document, err)
		}
	}
}

// A _dev value is env-expanded like any other value, and an override wins wherever it is written.
func TestDevSuffixExpandsEnvAndIgnoresOrder(t *testing.T) {
	t.Setenv("DBOSS_TEST_PORT", ":4321")
	path := filepath.Join(t.TempDir(), FileName)
	writeConfigFile(t, path, "proxy:\n  listen_dev: $DBOSS_TEST_PORT\n  listen: \":80\"\nprocfile:\n  web:\n    command: ./server\n    hosts: [demo.test]\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Proxy.Listen, ","); got != ":4321" {
		t.Fatalf("proxy.listen = %q, want :4321", got)
	}
}

// A dev session always has a console, on its loopback port, and does not need an admin list for
// it: the console signs a loopback request in by itself.
func TestDevSessionAlwaysHasAConsole(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	writeConfigFile(t, path, "procfile:\n  web:\n    command: ./server\n    hosts: [demo.test]\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ConsoleEnabled() {
		t.Fatal("a dev session should serve the console with no management block")
	}
	if got := cfg.ConsoleURL(); got != "" {
		t.Fatalf("ConsoleURL() = %q before the console is bound", got)
	}
	cfg.ConsolePort = 3104
	if got := cfg.ConsoleURL(); got != "http://127.0.0.1:3104" {
		t.Fatalf("ConsoleURL() = %q, want the loopback console", got)
	}
}

// A named console in a dev session still skips the admin list, and a public URL wins over the
// loopback one. A host without admins is still rejected.
func TestDevConsoleHostNeedsNoAdminEmails(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	writeConfigFile(t, path, "procfile:\n  web:\n    command: ./server\n    hosts: [demo.test]\nmanagement:\n  host: dboss.lvh.me\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ConsoleURL(); got != "https://dboss.lvh.me" {
		t.Fatalf("ConsoleURL() = %q, want the management host", got)
	}
	host := filepath.Join(t.TempDir(), FileName)
	writeConfigFile(t, host, "apps: ./apps\nmanagement:\n  host: dboss.lvh.me\n")
	if _, err := Load(host); err == nil || !strings.Contains(err.Error(), "management.admins") {
		t.Fatalf("a host console without admins should be rejected, got %v", err)
	}
}

// A host session with no management block has no console at all.
func TestHostWithoutManagementHasNoConsole(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	writeConfigFile(t, path, "apps: ./apps\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConsoleEnabled() || cfg.ConsoleURL() != "" {
		t.Fatalf("host console = %v %q, want off", cfg.ConsoleEnabled(), cfg.ConsoleURL())
	}
}
