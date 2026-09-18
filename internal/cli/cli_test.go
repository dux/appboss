package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"app-boss/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func TestLogOverlapTracksRollingTail(t *testing.T) {
	previous := []string{"a", "b", "c"}
	current := []string{"b", "c", "d"}
	if got := logOverlap(previous, current); got != 2 {
		t.Fatalf("got overlap %d", got)
	}
	if got := logOverlap(previous, []string{"x"}); got != 0 {
		t.Fatalf("got unrelated overlap %d", got)
	}
}

func TestCommonArgsExtractsSharedFlags(t *testing.T) {
	opts, err := commonArgs([]string{"demo", "--json", "-c", "x.yaml", "--socket", "s.sock", "-n", "5"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.json || opts.config != "x.yaml" || opts.socket != "s.sock" || strings.Join(opts.rest, " ") != "demo -n 5" {
		t.Fatalf("unexpected options: %+v", opts)
	}
	if _, err := commonArgs([]string{"--config"}); err == nil {
		t.Fatal("expected missing path error")
	}
}

func TestParseExecArgsStopsAtTheCommand(t *testing.T) {
	options, err := parseExecArgs([]string{"--timeout", "5m", "-c", "host.yaml", "demo", "/bin/sh", "-c", "exit 3", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if options.timeout != 5*time.Minute || options.configPath != "host.yaml" || options.json {
		t.Fatalf("unexpected options: %+v", options)
	}
	if strings.Join(options.rest, "|") != "demo|/bin/sh|-c|exit 3|--json" {
		t.Fatalf("rest = %v", options.rest)
	}
	if _, err := parseExecArgs([]string{"--config"}); err == nil {
		t.Fatal("expected missing value error")
	}
	if _, err := parseExecArgs([]string{"--timeout", "soon", "cmd"}); err == nil {
		t.Fatal("expected bad duration error")
	}
}

func TestFindConfigOrder(t *testing.T) {
	dir := chdir(t, t.TempDir())
	t.Setenv("APPBOSS_CONFIG", "")
	if _, err := findConfig(""); err == nil {
		t.Fatal("expected no config error")
	}
	writeFile(t, filepath.Join(dir, config.FileName), "apps: ./apps\n")
	if path, err := findConfig(""); err != nil || path != filepath.Join(dir, config.FileName) {
		t.Fatalf("got %q, %v", path, err)
	}
	writeFile(t, filepath.Join(dir, config.LocalFileName), "apps: ./apps\n")
	if path, err := findConfig(""); err != nil || path != filepath.Join(dir, config.LocalFileName) {
		t.Fatalf("local file should win: got %q, %v", path, err)
	}
	t.Setenv("APPBOSS_CONFIG", "/env/appboss.yaml")
	if path, _ := findConfig(""); path != "/env/appboss.yaml" {
		t.Fatalf("env should win over folder lookup: got %q", path)
	}
	if path, _ := findConfig("/flag/appboss.yaml"); path != "/flag/appboss.yaml" {
		t.Fatalf("flag should win over env: got %q", path)
	}
}

func TestAppArgumentDefaultsToFolderApp(t *testing.T) {
	dir := chdir(t, t.TempDir())
	t.Setenv("APPBOSS_CONFIG", "")
	if name, err := appArgument([]string{"explicit"}, ""); err != nil || name != "explicit" {
		t.Fatalf("got %q, %v", name, err)
	}
	writeFile(t, filepath.Join(dir, config.FileName), "procfile:\n  web: ./server\n")
	name, err := appArgument(nil, "")
	if err != nil || name != filepath.Base(dir) {
		t.Fatalf("got %q, %v", name, err)
	}
	writeFile(t, filepath.Join(dir, config.FileName), "apps: ./apps\n")
	if _, err := appArgument(nil, ""); err == nil {
		t.Fatal("host config must not supply an implicit app")
	}
}

func TestFindSocketFallsBackToWellKnownPath(t *testing.T) {
	dir := chdir(t, t.TempDir())
	t.Setenv("APPBOSS_CONFIG", "")
	t.Setenv("APPBOSS_SOCKET", "")
	if socket, _ := findSocket("", ""); socket != defaultSocket {
		t.Fatalf("got %q", socket)
	}
	writeFile(t, filepath.Join(dir, config.FileName), "procfile:\n  web: ./server\n")
	if socket, _ := findSocket("", ""); socket != defaultSocket {
		t.Fatalf("missing socket file should fall back, got %q", socket)
	}
	socketPath := filepath.Join(dir, ".appboss", "appboss.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, socketPath, "")
	if socket, _ := findSocket("", ""); socket != socketPath {
		t.Fatalf("existing config socket should win, got %q", socket)
	}
	t.Setenv("APPBOSS_SOCKET", "/env.sock")
	if socket, _ := findSocket("", ""); socket != "/env.sock" {
		t.Fatalf("env should win, got %q", socket)
	}
	if socket, _ := findSocket("/flag.sock", ""); socket != "/flag.sock" {
		t.Fatalf("flag should win, got %q", socket)
	}
}

func TestRenderUnitUsesResolvedPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	writeFile(t, path, "apps: ./apps\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	unit := renderUnit(cfg, "deploy", "", "/usr/local/bin/appboss")
	for _, want := range []string{"User=\"deploy\"\n", "WorkingDirectory=\"" + dir + "\"\n", "ExecStart=\"/usr/local/bin/appboss\" start -c \"" + path + "\"\n", "WantedBy=multi-user.target\n"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "Group=") {
		t.Fatalf("Group should be omitted by default:\n%s", unit)
	}
	if grouped := renderUnit(cfg, "deploy", "staff", "/usr/local/bin/appboss"); !strings.Contains(grouped, "Group=\"staff\"\n") {
		t.Fatalf("explicit group missing:\n%s", grouped)
	}
}

// chdir enters dir for the test and returns it with symlinks resolved, which is what os.Getwd
// reports (macOS puts temp dirs under /var -> /private/var).
func chdir(t *testing.T, dir string) string {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPasswordPrintsBcryptHash(t *testing.T) {
	var out, errOut strings.Builder
	cli := CLI{In: strings.NewReader("secret\n"), Out: &out, Err: &errOut}
	if code := cli.Run([]string{"password"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if err := bcrypt.CompareHashAndPassword([]byte(strings.TrimSpace(out.String())), []byte("secret")); err != nil {
		t.Fatalf("output is not a hash of the password: %q", out.String())
	}
	if code := (CLI{In: strings.NewReader("\n"), Out: &out, Err: &errOut}).Run([]string{"password"}); code == 0 {
		t.Fatal("empty password must fail")
	}
}

func TestConfigReferenceIsEmbedded(t *testing.T) {
	var out strings.Builder
	if code := (CLI{Out: &out, Err: io.Discard}).Run([]string{"config", "--reference"}); code != 0 || !strings.Contains(out.String(), "PART 1: <host>/appboss.yaml") {
		t.Fatalf("exit %d: %s", code, out.String())
	}
}

func TestHelpOutput(t *testing.T) {
	var out, errOut strings.Builder
	if code := (CLI{Out: &out, Err: &errOut}).Run(nil); code != 0 || !strings.Contains(out.String(), "maintenance") || !strings.Contains(out.String(), "Host session") {
		t.Fatalf("bare appboss: exit %d %s", code, out.String())
	}
	out.Reset()
	if code := (CLI{Out: &out, Err: &errOut}).Run([]string{"help", "logs"}); code != 0 || !strings.Contains(out.String(), "--process <name>") {
		t.Fatalf("help logs: exit %d %s", code, out.String())
	}
	out.Reset()
	if code := (CLI{Out: &out, Err: &errOut}).Run([]string{"logs", "--help"}); code != 0 || !strings.Contains(out.String(), "appboss logs [app]") {
		t.Fatalf("logs --help: exit %d %s", code, out.String())
	}
	if code := (CLI{Out: &out, Err: &errOut}).Run([]string{"nope"}); code != 2 || !strings.Contains(errOut.String(), `unknown command "nope"`) {
		t.Fatalf("unknown command: exit %d %s", code, errOut.String())
	}
	for _, name := range []string{"start", "systemd", "config", "check", "kill", "run", "stop", "restart", "status", "logs", "ls", "ports", "rescan", "maintenance", "password"} {
		if findCommand(name) == nil {
			t.Errorf("%s has no help entry", name)
		}
	}
}

func TestConfigPrintsGivenFileOrDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	writeFile(t, path, "# host\napps: ./apps\ndefaults:\n  idle_stop: 2h # never mind\n")
	var out strings.Builder
	if code := (CLI{Out: &out, Err: io.Discard}).Run([]string{"config", "-c", path}); code != 0 || out.String() != "# host\napps: ./apps\ndefaults:\n  idle_stop: 2h # never mind\n" {
		t.Fatalf("given: exit %d %q", code, out.String())
	}
	out.Reset()
	if code := (CLI{Out: &out, Err: io.Discard}).Run([]string{"config", "-c", path, "--defaults"}); code != 0 || !strings.Contains(out.String(), "\n  idle_stop: 2h0m0s\n") || !strings.Contains(out.String(), "\nports:\n  range:\n") {
		t.Fatalf("defaults: exit %d %s", code, out.String())
	}
	var errOut strings.Builder
	writeFile(t, path, "apps: ./apps\ndefaults:\n  idle_stpo: 2h\n")
	if code := (CLI{Out: io.Discard, Err: &errOut}).Run([]string{"config", "-c", path}); code != 1 || !strings.Contains(errOut.String(), "appboss.yaml:3: defaults.idle_stpo: unknown key\n  did you mean \"idle_stop\"?") {
		t.Fatalf("typo: exit %d %s", code, errOut.String())
	}
}
