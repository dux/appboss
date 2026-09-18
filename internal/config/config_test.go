package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	if cfg.App == nil || cfg.App.Procfile["web"] != "./server" || cfg.App.IdleStop != 0 || strings.Join(cfg.Proxy.Listen, ",") != "127.0.0.1:9090" {
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
	if _, err := LoadApp(path, Default().Defaults); err == nil || !strings.Contains(err.Error(), "proxy: is only valid in the root") {
		t.Fatalf("expected host-key error, got %v", err)
	}
}

func TestListKeysAcceptScalarOrSequence(t *testing.T) {
	dir := t.TempDir()
	defaults := Default().Defaults
	scalar, err := ParseApp([]byte("procfile:\n  web: ./server\nhosts: demo.test\nstatic_immutable: /packs/\nallow_ips: 10.0.0.0/8\n"), filepath.Join(dir, FileName), defaults)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := ParseApp([]byte("procfile:\n  web: ./server\nhosts: [demo.test]\nstatic_immutable: [/packs/]\nallow_ips: [10.0.0.0/8]\n"), filepath.Join(dir, FileName), defaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(scalar.Hosts) != 1 || !reflect.DeepEqual(scalar.Hosts, sequence.Hosts) || !reflect.DeepEqual(scalar.StaticImmutable, sequence.StaticImmutable) || !reflect.DeepEqual(scalar.AllowIPs, sequence.AllowIPs) {
		t.Fatalf("scalar %+v and sequence %+v differ", scalar, sequence)
	}

	if err := os.MkdirAll(filepath.Join(dir, "apps"), 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)
	writeConfigFile(t, path, "apps: ./apps\nproxy:\n  listen: [\":8080\", 127.0.0.1:8081]\n  trusted_cidrs: 10.0.0.0/8\nmanagement:\n  host: [boss.example.com, boss.internal]\n  auth:\n    admin_emails: admin@example.com\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Proxy.Listen, ",") != ":8080,127.0.0.1:8081" || strings.Join(cfg.Management.Host, ",") != "boss.example.com,boss.internal" || strings.Join(cfg.Proxy.TrustedCIDRs, ",") != "10.0.0.0/8" || strings.Join(cfg.Management.Auth.AdminEmails, ",") != "admin@example.com" {
		t.Fatalf("unexpected lists: listen=%v host=%v cidrs=%v emails=%v", cfg.Proxy.Listen, cfg.Management.Host, cfg.Proxy.TrustedCIDRs, cfg.Management.Auth.AdminEmails)
	}
	writeConfigFile(t, path, "apps: ./apps\nmanagement:\n  host: [boss.example.com, Boss.Example.com]\n  auth:\n    admin_emails: admin@example.com\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate management host = %v", err)
	}
	writeConfigFile(t, path, "apps: ./apps\nproxy:\n  listen: [\":8080\", \":8080\"]\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate listen address = %v", err)
	}
}

func TestManagementRequiresAuthAndProxyListener(t *testing.T) {
	cfg := Default()
	cfg.Apps = "/apps"
	cfg.Management.Host = List{"boss.example.com"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing admin email error")
	}
	cfg.Management.Auth.AdminEmails = []string{"admin@example.com"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Proxy.Listen = nil
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
	if got, err := ParseSize("1024"); err != nil || got != 1024 {
		t.Fatalf("plain bytes: got %d, %v", got, err)
	}
	if _, err := ParseSize("12x"); err == nil {
		t.Fatal("expected invalid unit error")
	}
}

func TestOverridesMirrorDefaults(t *testing.T) {
	for _, pair := range []struct {
		name               string
		defaults, override reflect.Type
	}{
		{"defaults", reflect.TypeOf(Defaults{}), reflect.TypeOf(Overrides{})},
		{"process", reflect.TypeOf(Process{}), reflect.TypeOf(ProcessOverrides{})},
		{"web", reflect.TypeOf(Web{}), reflect.TypeOf(WebOverrides{})},
	} {
		want, got := yamlFields(pair.defaults), yamlFields(pair.override)
		for key, field := range want {
			override, ok := got[key]
			if !ok {
				t.Errorf("%s: %s is missing from the override struct", pair.name, key)
				continue
			}
			expected := field.Type
			if expected.Kind() != reflect.Slice && expected.Kind() != reflect.Map {
				expected = reflect.PointerTo(expected)
			}
			if override.Type != expected || override.Name != field.Name {
				t.Errorf("%s: %s override is %s %s, want %s %s", pair.name, key, override.Name, override.Type, field.Name, expected)
			}
			delete(got, key)
		}
		for key := range got {
			t.Errorf("%s: override %s has no defaults field", pair.name, key)
		}
	}
}

// yamlFields flattens inline embedded structs the way the YAML decoder does, keyed by YAML name.
func yamlFields(typ reflect.Type) map[string]reflect.StructField {
	result := map[string]reflect.StructField{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Anonymous {
			for key, inner := range yamlFields(field.Type) {
				result[key] = inner
			}
			continue
		}
		if !field.IsExported() {
			continue
		}
		key, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		result[key] = field
	}
	return result
}

func TestAppOverridesMergeKeyByKey(t *testing.T) {
	defaults := Default().Defaults
	defaults.Env = map[string]string{"A": "host", "B": "host"}
	defaults.Headers = map[string]string{"X-Frame-Options": "DENY"}
	defaults.BasicAuth = map[string]string{"ops": "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"}
	data := "procfile:\n  web: ./server\nhosts: [demo.test, www.demo.test]\ncanonical_host: demo.test\nidle_stop: 0s\nenv:\n  B: app\nheaders:\n  X-Powered-By: \"\"\nstatic: ./public\nstatic_immutable: []\nmax_body: 50m\nallow_ips: [10.0.0.0/8]\nprocesses:\n  web:\n    env:\n      C: proc\n    stop_timeout: 1s\n"
	app, err := ParseApp([]byte(data), "app/dboss.yaml", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.IdleStop != 0 || app.Env["A"] != "host" || app.Env["B"] != "app" || app.Headers["X-Frame-Options"] != "DENY" || app.Headers["X-Powered-By"] != "" {
		t.Fatalf("unexpected merge: %+v", app.Defaults)
	}
	if _, ok := app.Headers["X-Powered-By"]; !ok {
		t.Fatal("empty header value must survive the merge")
	}
	if app.Static != "./public" || len(app.StaticImmutable) != 0 || int64(app.MaxBody) != 50<<20 || len(app.AllowPrefixes()) != 1 || app.BasicAuth["ops"] == "" {
		t.Fatalf("unexpected web keys: %+v", app.Web)
	}
	web := app.Process("web")
	if web.Env["C"] != "proc" || web.Env["B"] != "app" || web.StopTimeout.Value() != time.Second || app.Env["C"] != "" {
		t.Fatalf("unexpected process merge: %+v", web)
	}
	if defaults.Env["B"] != "host" || len(defaults.Headers) != 1 {
		t.Fatal("host defaults were mutated")
	}
}

func TestAppRejectsInvalidWebKeys(t *testing.T) {
	defaults := Default().Defaults
	for _, test := range []struct{ name, data, want string }{
		{"web key under process", "procfile:\n  web: ./server\nprocesses:\n  web:\n    static: ./public\n", "processes.web.static: unknown key"},
		{"canonical host", "procfile:\n  web: ./server\nhosts: [demo.test]\ncanonical_host: www.demo.test\n", "canonical_host"},
		{"allow ips", "procfile:\n  web: ./server\nallow_ips: [10.0.0.0]\n", "allow_ips"},
		{"basic auth", "procfile:\n  web: ./server\nbasic_auth:\n  alice: secret\n", "bcrypt"},
		{"header name", "procfile:\n  web: ./server\nheaders:\n  \"X Y\": z\n", "headers"},
	} {
		_, err := ParseApp([]byte(test.data), "dboss.yaml", defaults)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: got %v, want error containing %q", test.name, err, test.want)
		}
	}
}

func TestParseRootValidatesWithoutDisk(t *testing.T) {
	cfg, err := Parse([]byte("apps: ./apps\ndefaults:\n  static: ./public\n"), "/srv/dboss.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Apps != "/srv/apps" || cfg.Defaults.Static != "./public" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if _, err := Parse([]byte("apps: ./apps\ndefaults:\n  nope: 1\n"), "/srv/dboss.yaml"); err == nil {
		t.Fatal("expected unknown key error")
	}
}

func TestRestartRequiredListsHostKeys(t *testing.T) {
	old := Default()
	old.Apps = "/apps"
	current := old
	current.Defaults.IdleStop = Duration(time.Hour)
	if keys := RestartRequired(old, current); len(keys) != 0 {
		t.Fatalf("defaults change must apply live, got %v", keys)
	}
	current.Proxy.Listen = List{":81"}
	current.Ports.Range = [2]int{4000, 4100}
	if keys := RestartRequired(old, current); strings.Join(keys, ",") != "proxy,ports" {
		t.Fatalf("got %v", keys)
	}
}

func TestErrorsPointAtLineAndKey(t *testing.T) {
	for _, test := range []struct{ name, data, want, hint string }{
		{"typo", "apps: ./apps\ndefaults:\n  idle_stpo: 2h\n", "dboss.yaml:3: defaults.idle_stpo: unknown key", `did you mean "idle_stop"?`},
		{"duration", "apps: ./apps\ndefaults:\n  idle_stop: 2 hours\n", `dboss.yaml:3: defaults.idle_stop: invalid duration "2 hours"`, "durations look like"},
		{"enum", "apps: ./apps\ndefaults:\n  restart: sometimes\n", `dboss.yaml:3: defaults.restart: must be on-failure, always or never, not "sometimes"`, ""},
		{"listen", "apps: ./apps\nproxy:\n  listen: 80\n", `dboss.yaml:3: proxy.listen: invalid address "80"`, "host:port"},
		{"syntax", "apps: ./apps\ndefaults:\n  idle_stop: [1\n", "dboss.yaml:2: syntax error", ""},
	} {
		_, err := Parse([]byte(test.data), "/srv/dboss.yaml")
		if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), test.hint) {
			t.Errorf("%s: got %v, want %q with hint %q", test.name, err, test.want, test.hint)
		}
	}
	_, err := ParseApp([]byte("procfile:\n  web: ./x\nhost: [a.test]\n"), "/srv/apps/demo/dboss.yaml", Default().Defaults)
	if err == nil || !strings.Contains(err.Error(), `dboss.yaml:3: host: unknown key`) || !strings.Contains(err.Error(), `did you mean "hosts"?`) {
		t.Errorf("app typo: got %v", err)
	}
	var cfgErr *Error
	if !errors.As(err, &cfgErr) || cfgErr.Line != 3 || cfgErr.Key != "host" || cfgErr.Path != "/srv/apps/demo/dboss.yaml" {
		t.Errorf("structured error = %+v", cfgErr)
	}
}
