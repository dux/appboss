package cli

import (
	"bytes"
	"flag"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/ops"
	"dboss/internal/res"
	"dboss/internal/supervisor"
	"dboss/internal/version"

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
	t.Setenv("DBOSS_CONFIG", "")
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
	t.Setenv("DBOSS_CONFIG", "/env/dboss.yaml")
	if path, _ := findConfig(""); path != "/env/dboss.yaml" {
		t.Fatalf("env should win over folder lookup: got %q", path)
	}
	if path, _ := findConfig("/flag/dboss.yaml"); path != "/flag/dboss.yaml" {
		t.Fatalf("flag should win over env: got %q", path)
	}
}

func TestAppArgumentDefaultsToFolderApp(t *testing.T) {
	dir := chdir(t, t.TempDir())
	t.Setenv("DBOSS_CONFIG", "")
	if name, err := (&workdir{}).app([]string{"explicit"}); err != nil || name != "explicit" {
		t.Fatalf("got %q, %v", name, err)
	}
	writeFile(t, filepath.Join(dir, config.FileName), "procfile:\n  web: ./server\n")
	name, err := (&workdir{}).app(nil)
	if err != nil || name != filepath.Base(dir) {
		t.Fatalf("got %q, %v", name, err)
	}
	writeFile(t, filepath.Join(dir, config.FileName), "apps: ./apps\n")
	if _, err := (&workdir{}).app(nil); err == nil {
		t.Fatal("host config must not supply an implicit app")
	}
}

func TestFindSocketFallsBackToWellKnownPath(t *testing.T) {
	dir := chdir(t, t.TempDir())
	t.Setenv("DBOSS_CONFIG", "")
	t.Setenv("DBOSS_SOCKET", "")
	if socket, _ := (&workdir{}).socket(""); socket != defaultSocket {
		t.Fatalf("got %q", socket)
	}
	writeFile(t, filepath.Join(dir, config.FileName), "procfile:\n  web: ./server\n")
	if socket, _ := (&workdir{}).socket(""); socket != defaultSocket {
		t.Fatalf("missing socket file should fall back, got %q", socket)
	}
	socketPath := filepath.Join(dir, ".dboss", "dboss.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, socketPath, "")
	if socket, _ := (&workdir{}).socket(""); socket != socketPath {
		t.Fatalf("existing config socket should win, got %q", socket)
	}
	t.Setenv("DBOSS_SOCKET", "/env.sock")
	if socket, _ := (&workdir{}).socket(""); socket != "/env.sock" {
		t.Fatalf("env should win, got %q", socket)
	}
	if socket, _ := (&workdir{}).socket("/flag.sock"); socket != "/flag.sock" {
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
	unit := renderUnit(cfg, "deploy", "", "/usr/local/bin/dboss", "/home/deploy")
	for _, want := range []string{
		"User=deploy\n",
		"WorkingDirectory=" + dir + "\n",
		"ExecStart=\"/usr/local/bin/dboss\" start -c \"" + path + "\"\n",
		"WantedBy=multi-user.target\n",
		"Environment=PATH=/home/deploy/.local/bin:/home/deploy/bin:" + systemPATH + "\n",
		"ExecStartPre=+/bin/sh -c 'mkdir -p " + res.DefaultCgroupRoot + " && chown -R deploy " + res.DefaultCgroupRoot + " || true'\n",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "Group=") {
		t.Fatalf("Group should be omitted by default:\n%s", unit)
	}
	if grouped := renderUnit(cfg, "deploy", "staff", "/usr/local/bin/dboss", "/home/deploy"); !strings.Contains(grouped, "Group=staff\n") {
		t.Fatalf("explicit group missing:\n%s", grouped)
	}
	// An unknown user has no home, so the unit falls back to the plain system PATH instead of
	// naming a directory that does not exist.
	if homeless := renderUnit(cfg, "deploy", "", "/usr/local/bin/dboss", ""); !strings.Contains(homeless, "Environment=PATH="+systemPATH+"\n") {
		t.Fatalf("PATH without a home should be the system default:\n%s", homeless)
	}
}

// The install path always runs under sudo, so the current user is root there. Defaulting to it
// would put User=root in the unit and undo the point of the service user.
func TestDefaultServiceUserPrefersTheInvoker(t *testing.T) {
	t.Setenv("SUDO_USER", "deploy")
	name, err := defaultServiceUser()
	if err != nil {
		t.Fatal(err)
	}
	want := "deploy"
	if os.Geteuid() != 0 {
		current, err := user.Current()
		if err != nil {
			t.Fatal(err)
		}
		want = current.Username
	}
	if name != want {
		t.Fatalf("user = %q, want %q", name, want)
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
	if code := (CLI{Out: &out, Err: io.Discard}).Run([]string{"config", "--reference"}); code != 0 || !strings.Contains(out.String(), "PART 1: <host>/dboss.yaml") {
		t.Fatalf("exit %d: %s", code, out.String())
	}
}

func TestInitGeneratesTemplates(t *testing.T) {
	var out, errOut strings.Builder
	if code := (CLI{In: strings.NewReader(""), Out: &out, Err: &errOut}).Run([]string{"init", "app"}); code != 0 || !strings.Contains(out.String(), "# procfile:") {
		t.Fatalf("init app: exit %d %s", code, errOut.String())
	}
	out.Reset()
	if code := (CLI{In: strings.NewReader("2\n"), Out: &out, Err: &errOut}).Run([]string{"init"}); code != 0 || !strings.Contains(out.String(), "an app's dboss.yaml") {
		t.Fatalf("init prompt: exit %d %s", code, errOut.String())
	}
	out.Reset()
	if code := (CLI{In: strings.NewReader(""), Out: &out, Err: &errOut}).Run([]string{"init"}); code != 0 || !strings.Contains(out.String(), "root dboss.yaml") {
		t.Fatalf("init default: exit %d %s", code, errOut.String())
	}
	if code := (CLI{In: strings.NewReader(""), Out: &out, Err: &errOut}).Run([]string{"init", "nope"}); code == 0 {
		t.Fatal("init with an unknown type must fail")
	}
}

func TestHelpOutput(t *testing.T) {
	var out, errOut strings.Builder
	if code := (CLI{Out: &out, Err: &errOut}).Run(nil); code != 0 || !strings.Contains(out.String(), "maintenance") || !strings.Contains(out.String(), "Host session") {
		t.Fatalf("bare dboss: exit %d %s", code, out.String())
	}
	out.Reset()
	if code := (CLI{Out: &out, Err: &errOut}).Run([]string{"help", "logs"}); code != 0 || !strings.Contains(out.String(), "--process <name>") {
		t.Fatalf("help logs: exit %d %s", code, out.String())
	}
	out.Reset()
	if code := (CLI{Out: &out, Err: &errOut}).Run([]string{"logs", "--help"}); code != 0 || !strings.Contains(out.String(), "dboss logs [app]") {
		t.Fatalf("logs --help: exit %d %s", code, out.String())
	}
	if code := (CLI{Out: &out, Err: &errOut}).Run([]string{"nope"}); code != 2 || !strings.Contains(errOut.String(), `unknown command "nope"`) {
		t.Fatalf("unknown command: exit %d %s", code, errOut.String())
	}
	for _, name := range []string{"start", "systemd", "config", "check", "kill", "run", "stop", "restart", "destroy", "status", "logs", "ls", "ports", "rescan", "maintenance", "password", "version", "update"} {
		if findCommand(name) == nil {
			t.Errorf("%s has no help entry", name)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	var out, errOut strings.Builder
	if code := (CLI{Out: &out, Err: &errOut}).Run([]string{"version"}); code != 0 || strings.TrimSpace(out.String()) != version.String() {
		t.Fatalf("version: exit %d %q", code, out.String())
	}
}

func TestStartAlias(t *testing.T) {
	if cmd := findCommand("s"); cmd == nil || cmd.name != "start" {
		t.Fatalf("s does not resolve to start: %+v", cmd)
	}
	var out, errOut strings.Builder
	if code := (CLI{Out: &out, Err: &errOut}).Run([]string{"s", "--help"}); code != 0 || !strings.Contains(out.String(), "dboss start [-c path] [--login]") || !strings.Contains(out.String(), "Alias: s") {
		t.Fatalf("s --help: exit %d %s", code, out.String())
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
	if code := (CLI{Out: &out, Err: io.Discard}).Run([]string{"config", "-c", path, "--defaults"}); code != 0 || !strings.Contains(out.String(), "\n  idle_stop: 2h0m0s\n") || !strings.Contains(out.String(), "\nports:\n  - 3100\n") {
		t.Fatalf("defaults: exit %d %s", code, out.String())
	}
	var errOut strings.Builder
	writeFile(t, path, "apps: ./apps\ndefaults:\n  idle_stpo: 2h\n")
	if code := (CLI{Out: io.Discard, Err: &errOut}).Run([]string{"config", "-c", path}); code != 1 || !strings.Contains(errOut.String(), "dboss.yaml:3: defaults.idle_stpo: unknown key\n  did you mean \"idle_stop\"?") {
		t.Fatalf("typo: exit %d %s", code, errOut.String())
	}
}

func TestParseRemoteReadsFlagsAfterOperands(t *testing.T) {
	c := CLI{In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	opts, err := commonArgs([]string{"publish", "demo", "chat", "--event", "note", "--data", `{"a":1}`, "--process", "api"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := c.parseRemote("pubsub", opts, &workdir{})
	if err != nil {
		t.Fatal(err)
	}
	if request.Method != ops.ActionPubsubPublish || request.App != "demo" || request.Channel != "chat" || request.Event != "note" || request.Process != "api" || string(request.Data) != `{"a":1}` {
		t.Fatalf("request = %+v", request)
	}
	opts, _ = commonArgs([]string{"run", "demo", "deploy"})
	if request, err = c.parseRemote("hooks", opts, &workdir{}); err != nil || request.Method != ops.ActionHookRun || request.App != "demo" || request.Hook != "deploy" {
		t.Fatalf("hooks run = %+v, %v", request, err)
	}
	opts, _ = commonArgs([]string{"demo", "-n", "5", "-f"})
	if request, err = c.parseRemote("logs", opts, &workdir{}); err != nil || request.App != "demo" || request.Lines != 5 || !opts.follow {
		t.Fatalf("logs = %+v follow=%v, %v", request, opts.follow, err)
	}
}

// A hook without a ping URL says which keys give it one.
func TestPrintHumanHooksNamesTheMissingToken(t *testing.T) {
	var out bytes.Buffer
	c := CLI{Out: &out, Err: &out}
	hooks := []supervisor.HookInfo{{HookSnapshot: supervisor.HookSnapshot{Name: "deploy", Command: "./deploy.sh"}}}
	if err := c.printHuman(ops.ActionHook, hooks); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "set tokens.dboss") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestPubsubDataNormalizesPayloads(t *testing.T) {
	cases := []struct {
		value string
		input io.Reader
		want  string
	}{
		{"", nil, "null"},
		{"   ", nil, "null"},
		{`{"a":1}`, nil, `{"a":1}`},
		{"plain", nil, `"plain"`},
		{"-", strings.NewReader("42"), "42"},
	}
	for _, item := range cases {
		got, err := pubsubData(item.value, item.input)
		if err != nil {
			t.Fatalf("pubsubData(%q): %v", item.value, err)
		}
		if string(got) != item.want {
			t.Fatalf("pubsubData(%q) = %s, want %s", item.value, got, item.want)
		}
	}
}

func TestParseSubcommandFlagsReadsFlagsAfterTheOperand(t *testing.T) {
	// The order the help prints: operand first, flags after. flag.Parse alone stops at the operand.
	set := flag.NewFlagSet("pg drop", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	confirm := set.String("confirm", "", "")
	operands, err := parseSubcommandFlags(set, []string{"pr222_erpx", "--confirm", "pr222_erpx"})
	if err != nil {
		t.Fatal(err)
	}
	if len(operands) != 1 || operands[0] != "pr222_erpx" || *confirm != "pr222_erpx" {
		t.Fatalf("operands=%v confirm=%q", operands, *confirm)
	}

	// A bool flag between two operands must not swallow the one after it.
	set = flag.NewFlagSet("pg restore", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	target := set.String("target", "", "")
	force := set.Bool("force", false, "")
	operands, err = parseSubcommandFlags(set, []string{"backup-7", "--force", "--target", "scratch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(operands) != 1 || operands[0] != "backup-7" || !*force || *target != "scratch" {
		t.Fatalf("operands=%v force=%t target=%q", operands, *force, *target)
	}

	set = flag.NewFlagSet("pg drop", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.String("confirm", "", "")
	if _, err := parseSubcommandFlags(set, []string{"db", "--nope"}); err == nil {
		t.Fatal("an unknown flag should still be an error")
	}
}
