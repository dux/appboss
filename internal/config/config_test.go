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

func TestMatchHost(t *testing.T) {
	for _, test := range []struct {
		host, pattern string
		match         bool
	}{
		{"app.test", "app.test", true},
		{"a.dev.test", "*.dev.test", true},
		{"dev.test", "*.dev.test", false},
		{"dev.test", ".dev.test", true},
		{"a.dev.test", ".dev.test", true},
		{"a.b.dev.test", ".dev.test", true},
		{"dev.test.evil", ".dev.test", false},
		{"notdev.test", ".dev.test", false},
	} {
		if _, got := MatchHost(test.host, test.pattern); got != test.match {
			t.Errorf("MatchHost(%q, %q) = %v, want %v", test.host, test.pattern, got, test.match)
		}
	}
}

func TestCanonicalHostAcceptsAShorthandPattern(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	defaults := Default().Defaults
	if _, err := ParseApp([]byte("procfile:\n  web:\n    command: ./server\n    domains: [\".demo.test\"]\ncanonical_host: demo.test\n"), path, defaults); err != nil {
		t.Fatalf("canonical host covered by shorthand: %v", err)
	}
	if _, err := ParseApp([]byte("procfile:\n  web:\n    command: ./server\n    domains: [\".demo.test\"]\ncanonical_host: other.test\n"), path, defaults); err == nil {
		t.Fatal("canonical host outside domains was accepted")
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
	writeConfigFile(t, path, "procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\nidle_stop: 0s\nproxy:\n  listen: 127.0.0.1:9090\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.App == nil || cfg.App.Procfile["web"].Command != "./server" || cfg.App.IdleStop != 0 || strings.Join(cfg.Proxy.Listen, ",") != "127.0.0.1:9090" {
		t.Fatalf("unexpected single-app config: %+v app=%+v", cfg, cfg.App)
	}
	if cfg.Apps != "" {
		t.Fatalf("apps should be empty in single mode, got %q", cfg.Apps)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsAmbiguousRole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	writeConfigFile(t, path, "procfile:\n  web: ./server\napps: ./apps\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("expected both-set error, got %v", err)
	}
	// A file with neither key is a host that scans the default apps directory.
	writeConfigFile(t, path, "defaults:\n  idle_stop: 1h\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.App != nil || cfg.Apps != filepath.Join(dir, "apps") {
		t.Fatalf("empty file should be a host with ./apps: app=%v apps=%q", cfg.App, cfg.Apps)
	}
}

func TestManagementPublicURLDerivesFromHost(t *testing.T) {
	cfg := Default()
	if url := cfg.Management.PublicURL(); url != "" {
		t.Fatalf("disabled management url = %q", url)
	}
	cfg.Management.Host = List{"dboss.example.com", "dboss.internal"}
	if url := cfg.Management.PublicURL(); url != "https://dboss.example.com" {
		t.Fatalf("derived management url = %q", url)
	}
	cfg.Management.URL = "http://dboss.example.com/"
	if url := cfg.Management.PublicURL(); url != "http://dboss.example.com" {
		t.Fatalf("url override = %q", url)
	}
}

func TestCloudflareOnlyLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	writeConfigFile(t, path, "proxy:\n  cloudflare_only: true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Proxy.CloudflareOnly {
		t.Fatal("cloudflare_only did not load")
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
	writeConfigFile(t, path, "procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n")
	app, err := LoadApp(path, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.Procfile["web"].Command != "./server" {
		t.Fatalf("unexpected procfile: %#v", app.Procfile)
	}
	writeConfigFile(t, path, "procfile:\n  web: ./server\nproxy:\n  listen: 127.0.0.1:9090\n")
	if _, err := LoadApp(path, Default().Defaults); err == nil || !strings.Contains(err.Error(), "proxy: is only valid in the root") {
		t.Fatalf("expected host-key error, got %v", err)
	}
}

func TestParseAppAutostart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	defaults := Default().Defaults
	omitted, err := ParseApp([]byte("procfile:\n  web: ./server\n"), path, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if omitted.Autostart != AutostartOn {
		t.Fatalf("omitted autostart = %q, want %q", omitted.Autostart, AutostartOn)
	}
	off, err := ParseApp([]byte("procfile:\n  web: ./server\nautostart: false\n"), path, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if off.Autostart != AutostartOff {
		t.Fatalf("autostart: false = %q, want %q", off.Autostart, AutostartOff)
	}
	on, err := ParseApp([]byte("procfile:\n  web: ./server\nautostart: true\n"), path, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if on.Autostart != AutostartOn {
		t.Fatalf("autostart: true = %q, want %q", on.Autostart, AutostartOn)
	}
	button, err := ParseApp([]byte("procfile:\n  web: ./server\nautostart: button\n"), path, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if button.Autostart != AutostartButton || button.Autostart.Starts() {
		t.Fatalf("autostart: button = %q starts=%v, want button false", button.Autostart, button.Autostart.Starts())
	}
	if _, err := ParseApp([]byte("procfile:\n  web: ./server\nautostart: maybe\n"), path, defaults); err == nil {
		t.Fatal("autostart: maybe should be rejected")
	}
}

func TestParseAppDeletableDefaultsToFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	omitted, err := ParseApp([]byte("procfile:\n  web: ./server\n"), path, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if omitted.Deletable {
		t.Fatal("omitted deletable should be false")
	}
	enabled, err := ParseApp([]byte("procfile:\n  web: ./server\ndeletable: true\n"), path, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.Deletable {
		t.Fatal("deletable: true did not load")
	}
}

func TestListKeysAcceptScalarOrSequence(t *testing.T) {
	dir := t.TempDir()
	defaults := Default().Defaults
	scalar, err := ParseApp([]byte("procfile:\n  web:\n    command: ./server\n    domains: demo.test\nstatic_immutable: /packs/\nallow_ips: 10.0.0.0/8\n"), filepath.Join(dir, FileName), defaults)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := ParseApp([]byte("procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\nstatic_immutable: [/packs/]\nallow_ips: [10.0.0.0/8]\n"), filepath.Join(dir, FileName), defaults)
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
	writeConfigFile(t, path, "apps: ./apps\nproxy:\n  listen: [\":8080\", 127.0.0.1:8081]\n  trusted_cidrs: 10.0.0.0/8\nmanagement:\n  host: [dboss.example.com, dboss.internal]\n  auth:\n    admin_emails: admin@example.com\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Proxy.Listen, ",") != ":8080,127.0.0.1:8081" || strings.Join(cfg.Management.Host, ",") != "dboss.example.com,dboss.internal" || strings.Join(cfg.Proxy.TrustedCIDRs, ",") != "10.0.0.0/8" || strings.Join(cfg.Management.Auth.AdminEmails, ",") != "admin@example.com" {
		t.Fatalf("unexpected lists: listen=%v host=%v cidrs=%v emails=%v", cfg.Proxy.Listen, cfg.Management.Host, cfg.Proxy.TrustedCIDRs, cfg.Management.Auth.AdminEmails)
	}
	writeConfigFile(t, path, "apps: ./apps\nmanagement:\n  host: [dboss.example.com, dboss.Example.com]\n  auth:\n    admin_emails: admin@example.com\n")
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
	cfg.Management.Host = List{"dboss.example.com"}
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

func TestManagementURLMustMatchAHost(t *testing.T) {
	cfg := Default()
	cfg.Apps = "/apps"
	cfg.Management.Host = List{"dboss.example.com"}
	cfg.Management.Auth.AdminEmails = []string{"admin@example.com"}
	for value, wantErr := range map[string]string{
		"https://dboss.example.com":      "",
		"http://dboss.Example.com:8080/": "",
		"dboss.example.com":              "invalid URL",
		"ftp://dboss.example.com":        "invalid URL",
		"https://other.example.com":      "not one of management.host",
	} {
		cfg.Management.URL = value
		err := cfg.Validate()
		if wantErr == "" && err != nil {
			t.Errorf("%q: unexpected error %v", value, err)
		}
		if wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)) {
			t.Errorf("%q: error = %v, want %q", value, err, wantErr)
		}
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
				if !sameStructShape(override.Type, expected) {
					t.Errorf("%s: %s override is %s %s, want %s %s", pair.name, key, override.Name, override.Type, field.Name, expected)
				}
			}
			delete(got, key)
		}
		for key := range got {
			t.Errorf("%s: override %s has no defaults field", pair.name, key)
		}
	}
}

// sameStructShape reports whether two types are pointers to structs with the same YAML keys, so a
// nested shared block can use a dedicated pointer-field override type.
func sameStructShape(a, b reflect.Type) bool {
	for a.Kind() == reflect.Pointer {
		a = a.Elem()
	}
	for b.Kind() == reflect.Pointer {
		b = b.Elem()
	}
	if a.Kind() != reflect.Struct || b.Kind() != reflect.Struct {
		return false
	}
	left, right := yamlFields(a), yamlFields(b)
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if _, ok := right[key]; !ok {
			return false
		}
	}
	return true
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
		if key == "" || key == "-" {
			continue
		}
		result[key] = field
	}
	return result
}

func TestAppOverridesMergeKeyByKey(t *testing.T) {
	defaults := Default().Defaults
	defaults.Env = map[string]string{"A": "host", "B": "host"}
	defaults.Headers = map[string]string{"X-Frame-Options": "DENY"}
	defaults.BasicAuth = map[string]string{"ops": "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"}
	data := "procfile:\n  web:\n    command: ./server\n    domains: [demo.test, www.demo.test]\ncanonical_host: demo.test\nidle_stop: 0s\nenv:\n  B: app\nheaders:\n  X-Powered-By: \"\"\nstatic: ./public\nstatic_immutable: []\nmax_body: 50m\nallow_ips: [10.0.0.0/8]\nprocesses:\n  web:\n    env:\n      C: proc\n    stop_timeout: 1s\n"
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

func TestUnhealthyThreshold(t *testing.T) {
	if got := Default().Defaults.UnhealthyThreshold; got != 3 {
		t.Fatalf("default unhealthy_threshold = %d, want 3", got)
	}
	defaults := Default().Defaults
	app, err := ParseApp([]byte("procfile:\n  web: ./server\nunhealthy_threshold: 5\nprocesses:\n  web:\n    unhealthy_threshold: 0\n"), "dboss.yaml", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.UnhealthyThreshold != 5 {
		t.Fatalf("app unhealthy_threshold = %d, want 5", app.UnhealthyThreshold)
	}
	if got := app.Process("web").UnhealthyThreshold; got != 0 {
		t.Fatalf("process unhealthy_threshold = %d, want 0", got)
	}
	_, err = ParseApp([]byte("procfile:\n  web: ./server\nunhealthy_threshold: -1\n"), "dboss.yaml", defaults)
	if err == nil || !strings.Contains(err.Error(), "unhealthy_threshold") {
		t.Fatalf("negative unhealthy_threshold: got %v", err)
	}
}

func TestWebHealthPath(t *testing.T) {
	defaults := Default().Defaults
	app, err := ParseApp([]byte("procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n    health: /up\n  worker: ./worker.sh\n"), "dboss.yaml", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if got := app.Process("web").Health; got != "/up" {
		t.Fatalf("web health = %q, want /up", got)
	}
	if got := app.Process("worker").Health; got != "tcp" {
		t.Fatalf("worker health = %q, want tcp", got)
	}
	for name, data := range map[string]string{
		"worker":    "procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n  worker:\n    command: ./worker.sh\n    health: /up\n",
		"scheme":    "procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n    health: http:/up\n",
		"app level": "procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\nhealth: /up\n",
		"process":   "procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\nprocesses:\n  web:\n    health: /up\n",
	} {
		if _, err := ParseApp([]byte(data), "dboss.yaml", defaults); err == nil {
			t.Errorf("%s health should be rejected", name)
		}
	}
}

func TestAppRejectsInvalidWebKeys(t *testing.T) {
	defaults := Default().Defaults
	for _, test := range []struct{ name, data, want string }{
		{"web key under process", "procfile:\n  web: ./server\nprocesses:\n  web:\n    static: ./public\n", "processes.web.static: unknown key"},
		{"canonical host", "procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\ncanonical_host: www.demo.test\n", "canonical_host"},
		{"allow ips", "procfile:\n  web: ./server\nallow_ips: [10.0.0.0]\n", "allow_ips"},
		{"basic auth", "procfile:\n  web: ./server\nbasic_auth:\n  alice: secret\n", "bcrypt"},
		{"header name", "procfile:\n  web: ./server\nheaders:\n  \"X Y\": z\n", "headers"},
		{"auth email", "procfile:\n  web: ./server\nauth:\n  allow_emails: [not-an-email]\n", "auth.allow_emails"},
		{"auth domain pattern", "procfile:\n  web: ./server\nauth:\n  allow_emails: [\"*@bad domain\"]\n", "auth.allow_emails"},
		{"auth duplicate", "procfile:\n  web: ./server\nauth:\n  allow_emails: [a@b.com, A@B.com]\n", "duplicate"},
		{"auth session ttl", "procfile:\n  web: ./server\nauth:\n  session_ttl: 0s\n", "auth.session_ttl"},
		{"authcog empty path", "procfile:\n  web: ./server\nauthcog:\n  login: true\n  path: \"\"\n", "authcog.path"},
		{"authcog bad path", "procfile:\n  web: ./server\nauthcog:\n  login: true\n  path: bad\n", "authcog.path"},
		{"authcog path collision", "procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n    pubsub: /authcog\nauthcog:\n  login: true\n", "authcog.path"},
		{"authcog bad realm", "procfile:\n  web: ./server\nauthcog:\n  login: true\n  realm: a.b\n", "authcog.realm"},
		{"alerts window", "procfile:\n  web: ./server\nalerts:\n  window: 0s\n", "alerts.window"},
		{"alerts error rate", "procfile:\n  web: ./server\nalerts:\n  error_rate: 101\n", "alerts.error_rate"},
		{"alerts slow p95", "procfile:\n  web: ./server\nalerts:\n  slow_p95: -1s\n", "alerts.slow_p95"},
	} {
		_, err := ParseApp([]byte(test.data), "dboss.yaml", defaults)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: got %v, want error containing %q", test.name, err, test.want)
		}
	}
}

func TestAuthAllowsEmailsAndDomains(t *testing.T) {
	defaults := Default().Defaults
	defaults.Auth.AllowEmails = List{"ops@host.test"}
	open, err := ParseApp([]byte("procfile:\n  web: ./server\nauth:\n  allow_emails: []\n"), "dboss.yaml", defaults)
	if err != nil || open.Auth.Enabled() {
		t.Fatalf("an empty app list must replace the host list: %v %+v", err, open.Auth)
	}
	app, err := ParseApp([]byte("procfile:\n  web: ./server\nauth:\n  allow_emails: [Ana@Example.com, \"*@team.test\"]\n"), "dboss.yaml", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if !app.Auth.Enabled() || app.Auth.SessionTTL.Value() != 24*time.Hour {
		t.Fatalf("unexpected auth: %+v", app.Auth)
	}
	for email, want := range map[string]bool{"ana@example.com": true, "ANA@example.com": true, "bo@team.test": true, "bo@sub.team.test": false, "ops@host.test": false, "eve@example.com": false, "team.test": false} {
		if got := app.Auth.Allows(email); got != want {
			t.Errorf("Allows(%q) = %v, want %v", email, got, want)
		}
	}
}

func TestAuthCogOverrideKeyByKey(t *testing.T) {
	defaults := Default().Defaults
	defaults.AuthCog.Login = true
	app, err := ParseApp([]byte("procfile:\n  web: ./server\nauthcog:\n  realm: shop\n"), "dboss.yaml", defaults)
	if err != nil {
		t.Fatal(err)
	}
	want := AuthCog{Login: true, Path: "/authcog", Realm: "shop"}
	if app.AuthCog != want || !app.AuthCog.Enabled() || app.AuthCog.RealmHost() != "shop.authcog.com" {
		t.Fatalf("authcog = %+v, want %+v", app.AuthCog, want)
	}
	if got := Default().Defaults.AuthCog; got != (AuthCog{Path: "/authcog", Realm: "auth"}) {
		t.Fatalf("unexpected authcog defaults: %+v", got)
	}
}

func TestAlertsOverrideKeyByKey(t *testing.T) {
	defaults := Default().Defaults
	defaults.Alerts.ErrorRate = 25
	app, err := ParseApp([]byte("procfile:\n  web: ./server\nalerts:\n  slow_p95: 2s\n"), "dboss.yaml", defaults)
	if err != nil {
		t.Fatal(err)
	}
	want := Alerts{Window: Duration(5 * time.Minute), MinRequests: 20, ErrorRate: 25, SlowP95: Duration(2 * time.Second)}
	if app.Alerts != want || !app.Alerts.Enabled() {
		t.Fatalf("alerts = %+v, want %+v", app.Alerts, want)
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

func TestEnvExpansion(t *testing.T) {
	t.Setenv("DBOSS_TEST_HOST", "myapp.com")
	t.Setenv("DBOSS_TEST_COUNT", "2")
	t.Setenv("DBOSS_TEST_IDLE", "90s")
	t.Setenv("DBOSS_TEST_MAX", "512m")

	const hash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	cfg, err := Parse([]byte(`apps: ./apps
state_dir: /var/lib/$DBOSS_TEST_HOST
proxy:
  listen: [":8080"]
  wake:
    retry_after: $DBOSS_TEST_COUNT
management:
  host: [$DBOSS_TEST_HOST]
  auth:
    admin_emails: [admin@example.com]
defaults:
  idle_stop: $DBOSS_TEST_IDLE
  memory_max: $DBOSS_TEST_MAX
  headers:
    X-Test: $lower $1 $DBOSS_TEST_UNSET
  env:
    MALLOC_ARENA_MAX: $DBOSS_TEST_COUNT
  basic_auth:
    ops: `+hash+`
`), "/srv/dboss.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateDir != "/var/lib/myapp.com" {
		t.Errorf("state_dir = %q", cfg.StateDir)
	}
	if cfg.Proxy.Wake.RetryAfter != 2 {
		t.Errorf("retry_after = %d, want 2", cfg.Proxy.Wake.RetryAfter)
	}
	if cfg.Management.Host[0] != "myapp.com" {
		t.Errorf("management.host = %v", cfg.Management.Host)
	}
	if cfg.Defaults.IdleStop.Value() != 90*time.Second || cfg.Defaults.MemoryMax != Size(512<<20) {
		t.Errorf("idle_stop/memory_max = %v/%v", cfg.Defaults.IdleStop, cfg.Defaults.MemoryMax)
	}
	if cfg.Defaults.Headers["X-Test"] != "$lower $1 $DBOSS_TEST_UNSET" {
		t.Errorf("unset/lowercase must stay literal, got %q", cfg.Defaults.Headers["X-Test"])
	}
	if cfg.Defaults.Env["MALLOC_ARENA_MAX"] != "2" {
		t.Errorf("numeric env into a string map = %q, want \"2\"", cfg.Defaults.Env["MALLOC_ARENA_MAX"])
	}
	if cfg.Defaults.BasicAuth["ops"] != hash {
		t.Errorf("bcrypt hash was rewritten: %q", cfg.Defaults.BasicAuth["ops"])
	}
}

func TestEnvExpansionSkipsCommands(t *testing.T) {
	t.Setenv("DBOSS_TEST_PORT", "7777")
	app, err := ParseApp([]byte(`procfile:
  web: run --port $DBOSS_TEST_PORT
cron:
  tick:
    schedule: every 5m
    command: run $DBOSS_TEST_PORT
`), "/srv/apps/demo/dboss.yaml", Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.Procfile["web"].Command != "run --port $DBOSS_TEST_PORT" {
		t.Errorf("procfile expanded: %q", app.Procfile["web"].Command)
	}
	if app.Cron["tick"].Command != "run $DBOSS_TEST_PORT" {
		t.Errorf("cron command expanded: %q", app.Cron["tick"].Command)
	}
}

func TestStdoutRetentionDefaultsAndValidates(t *testing.T) {
	if got := Default().Defaults.StdoutRetention.Value(); got != 3*time.Hour {
		t.Fatalf("stdout_retention default = %v, want 3h", got)
	}
	cfg, err := Parse([]byte("apps: ./apps\ndefaults:\n  stdout_retention: 6h\n"), "/srv/dboss.yaml")
	if err != nil || cfg.Defaults.StdoutRetention.Value() != 6*time.Hour {
		t.Fatalf("stdout_retention override: %v %v", err, cfg.Defaults.StdoutRetention.Value())
	}
	if _, err := Parse([]byte("apps: ./apps\ndefaults:\n  stdout_retention: -1h\n"), "/srv/dboss.yaml"); err == nil {
		t.Fatal("negative stdout_retention should fail")
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
	_, err := ParseApp([]byte("procfile:\n  web: ./x\ncanonical_hst: a.test\n"), "/srv/apps/demo/dboss.yaml", Default().Defaults)
	if err == nil || !strings.Contains(err.Error(), `dboss.yaml:3: canonical_hst: unknown key`) || !strings.Contains(err.Error(), `did you mean "canonical_host"?`) {
		t.Errorf("app typo: got %v", err)
	}
	var cfgErr *Error
	if !errors.As(err, &cfgErr) || cfgErr.Line != 3 || cfgErr.Key != "canonical_hst" || cfgErr.Path != "/srv/apps/demo/dboss.yaml" {
		t.Errorf("structured error = %+v", cfgErr)
	}
}

func TestPubsubConfig(t *testing.T) {
	defaults := Default().Defaults
	if defaults.Pubsub.Replay != 10 || defaults.Pubsub.MaxClients != 500 || !defaults.Pubsub.ClientEvents {
		t.Fatalf("unexpected pubsub defaults: %+v", defaults.Pubsub)
	}

	app, err := ParseApp([]byte("procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n    pubsub:\n      path: /socketio\n      replay: 0\n"), "dboss.yaml", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.Pubsub.Path != "/socketio" || app.Pubsub.Replay != 0 {
		t.Fatalf("unexpected pubsub: %+v", app.Pubsub)
	}
	// An absent key in the process block keeps the value from defaults.
	if app.Pubsub.MaxClients != 500 || !app.Pubsub.ClientEvents || app.Pubsub.MaxMessageSize != Size(64<<10) {
		t.Fatalf("override did not merge with defaults: %+v", app.Pubsub)
	}

	// `pubsub: true` is the default path; a bare string sets a custom one.
	shorthand, err := ParseApp([]byte("procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n    pubsub: true\n"), "dboss.yaml", defaults)
	if err != nil || shorthand.Pubsub.Path != DefaultPubsubPath {
		t.Fatalf("pubsub: true = %+v, %v", shorthand.Pubsub, err)
	}
	custom, err := ParseApp([]byte("procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n    pubsub: /events\n"), "dboss.yaml", defaults)
	if err != nil || custom.Pubsub.Path != "/events" || custom.Pubsub.Replay != 10 {
		t.Fatalf("pubsub: /events = %+v, %v", custom.Pubsub, err)
	}

	// The top-level block is gone, and pubsub is web-process-only.
	for _, data := range []string{
		"procfile:\n  web: ./server\npubsub:\n  path: /socketio\n",
		"procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n  worker:\n    command: ./jobs\n    pubsub: true\n",
		"procfile:\n  web:\n    command: ./server\n    pubsub: true\n",
	} {
		if _, err := ParseApp([]byte(data), "dboss.yaml", defaults); err == nil {
			t.Errorf("expected an error for:\n%s", data)
		}
	}

	for _, path := range []string{"/", "socketio", "/socketio/", "/socket io", "/a//b", "/a$b"} {
		data := "procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n    pubsub: \"" + path + "\"\n"
		if _, err := ParseApp([]byte(data), "dboss.yaml", defaults); err == nil {
			t.Errorf("path %q should be invalid", path)
		}
	}
}

func TestProcessSpecShapesAndKeys(t *testing.T) {
	defaults := Default().Defaults
	// A scalar is a background process and does not make the app a web app.
	app, err := ParseApp([]byte("procfile:\n  worker: ./jobs\n"), "dboss.yaml", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.WebProcess != "" || len(app.Hosts) != 0 {
		t.Fatalf("a scalar process must not be the web process: %+v", app)
	}
	// An unknown process key and an unknown pubsub key are both rejected.
	for _, data := range []string{
		"procfile:\n  web:\n    command: ./server\n    ports: [80]\n",
		"procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n    pubsub:\n      route: /socketio\n",
		"procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n  api:\n    command: ./api\n    domains: [api.demo.test]\n",
	} {
		if _, err := ParseApp([]byte(data), "dboss.yaml", defaults); err == nil {
			t.Errorf("expected an error for:\n%s", data)
		}
	}
	// pubsub: false is off and may sit on any process.
	if _, err := ParseApp([]byte("procfile:\n  web:\n    command: ./server\n    domains: [demo.test]\n  worker:\n    command: ./jobs\n    pubsub: false\n"), "dboss.yaml", defaults); err != nil {
		t.Fatalf("pubsub: false should be off: %v", err)
	}
}

func TestSingleAppModeBindsDevDomain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	writeConfigFile(t, path, "procfile:\n  web: ./server\n  worker: ./jobs\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.App == nil || cfg.App.WebProcess != "web" || !reflect.DeepEqual(cfg.App.Hosts, List{DevDomain}) {
		t.Fatalf("single app should bind %s to web: %+v", DevDomain, cfg.App)
	}
	// An explicit domain is kept and no dev domain is added.
	writeConfigFile(t, path, "procfile:\n  web:\n    command: ./server\n    domains: [my.test]\n")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.App.Hosts, List{"my.test"}) {
		t.Fatalf("explicit domains must win: %+v", cfg.App.Hosts)
	}
	// pubsub with no domains is fine in single mode: the dev domain serves it.
	writeConfigFile(t, path, "procfile:\n  web:\n    command: ./server\n    pubsub: true\n")
	if _, err := Load(path); err != nil {
		t.Fatalf("single-mode pubsub without domains: %v", err)
	}
	// health with no domains binds the dev domain first, then resolves onto the web process.
	writeConfigFile(t, path, "procfile:\n  web:\n    command: ./server\n    health: /up\n")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("single-mode health without domains: %v", err)
	}
	if got := cfg.App.Process("web").Health; got != "/up" {
		t.Fatalf("single-mode health = %q, want /up", got)
	}
}
