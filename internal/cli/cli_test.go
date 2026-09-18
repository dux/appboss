package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"deploy-boss/internal/config"
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
	t.Setenv("DBOSS_CONFIG", "")
	t.Setenv("DBOSS_SOCKET", "")
	if socket, _ := findSocket("", ""); socket != defaultSocket {
		t.Fatalf("got %q", socket)
	}
	writeFile(t, filepath.Join(dir, config.FileName), "procfile:\n  web: ./server\n")
	if socket, _ := findSocket("", ""); socket != defaultSocket {
		t.Fatalf("missing socket file should fall back, got %q", socket)
	}
	socketPath := filepath.Join(dir, ".dboss", "dboss.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, socketPath, "")
	if socket, _ := findSocket("", ""); socket != socketPath {
		t.Fatalf("existing config socket should win, got %q", socket)
	}
	t.Setenv("DBOSS_SOCKET", "/env.sock")
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
	unit := renderUnit(cfg, "deploy", "/usr/local/bin/dboss")
	for _, want := range []string{"User=deploy\n", "WorkingDirectory=" + dir + "\n", "ExecStart=/usr/local/bin/dboss start -c " + path + "\n", "WantedBy=multi-user.target\n"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
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
