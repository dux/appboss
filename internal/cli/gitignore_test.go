package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/config"
)

// gitRepo is a throwaway checkout with a .gitignore, which is the only case the warning speaks
// up in. Without git on the box there is nothing to assert, so the test says so and stops.
func gitRepo(t *testing.T, ignore string) config.Config {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	command := exec.Command("git", "init", "-q")
	command.Dir = dir
	if err := command.Run(); err != nil {
		t.Skipf("git init failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(ignore), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, config.FileName)
	writeFile(t, path, "apps: ./apps\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestWarnsWhenTheRuntimeFolderWouldBeCommitted(t *testing.T) {
	cfg := gitRepo(t, "node_modules\n")
	var out bytes.Buffer
	warnUnignoredRuntime(&out, cfg)
	if !strings.Contains(out.String(), ".dboss") || !strings.Contains(out.String(), "not gitignored") {
		t.Fatalf("expected a warning about .dboss, got %q", out.String())
	}
	if !strings.Contains(out.String(), "echo .dboss/ >>") {
		t.Fatalf("warning should say how to fix it, got %q", out.String())
	}
}

// Every rule style has to count, and on a first start the folder does not exist yet, so a
// directory-only rule like `.dboss/` must still be recognised.
func TestQuietWhenTheRuntimeFolderIsIgnored(t *testing.T) {
	for _, rule := range []string{".dboss/\n", ".dboss\n", "/.dboss\n", "node_modules\n.dboss/\n"} {
		cfg := gitRepo(t, rule)
		if _, err := os.Stat(cfg.StateDir); err == nil {
			t.Fatal("the runtime folder should not exist yet in this test")
		}
		var out bytes.Buffer
		warnUnignoredRuntime(&out, cfg)
		if out.Len() != 0 {
			t.Fatalf("rule %q should silence the warning, got %q", rule, out.String())
		}
	}
}

// A repository that keeps no .gitignore is not asking for advice, so the warning stays out of
// the way even though .dboss is plainly untracked there.
func TestQuietWithoutAGitignore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	writeFile(t, path, "apps: ./apps\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warnUnignoredRuntime(&out, cfg)
	if out.Len() != 0 {
		t.Fatalf("no .gitignore should mean no warning, got %q", out.String())
	}
}

// A host that keeps its state outside the checkout, which is every real server, has nothing in
// the repository to ignore.
func TestRuntimeRootsSkipsPathsOutsideTheConfigDir(t *testing.T) {
	cfg := config.Config{Dir: "/srv/dboss", StateDir: "/var/lib/dboss/state", LogDir: "/var/log/dboss", Socket: "/run/dboss/dboss.sock"}
	if roots := runtimeRoots(cfg); len(roots) != 0 {
		t.Fatalf("roots = %v, want none", roots)
	}
}

// state_dir, log_dir and the socket all default under one .dboss, and the operator should be
// told about it once.
func TestRuntimeRootsCollapseToOneEntry(t *testing.T) {
	cfg := config.Config{Dir: "/srv/app", StateDir: "/srv/app/.dboss/state", LogDir: "/srv/app/.dboss/log", Socket: "/srv/app/.dboss/dboss.sock"}
	roots := runtimeRoots(cfg)
	if len(roots) != 1 || roots[0] != ".dboss" {
		t.Fatalf("roots = %v, want [.dboss]", roots)
	}
}
