package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/config"
)

func TestNewCommandKeepsTheWholeLine(t *testing.T) {
	command := newCommand("web", "  ./server --port x ")
	if command.Line != "./server --port x" || command.Argv != nil {
		t.Fatalf("unexpected command: %#v", command)
	}
}

func TestLoadEnv(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/env"
	writeTestFile(t, path, "A=one\nexport B=two\nC=\"three words\"\nD='${A}'\n")
	values, err := loadEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["C"] != "three words" || values["D"] != "${A}" {
		t.Fatalf("unexpected env: %#v", values)
	}
}

func TestDiscoverWalksAppsDirectory(t *testing.T) {
	root := t.TempDir()
	appsDir := filepath.Join(root, "apps")
	realApp := filepath.Join(root, "checkouts", "real")
	for _, dir := range []string{appsDir, realApp, filepath.Join(appsDir, "plain"), filepath.Join(appsDir, ".hidden")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(realApp, config.FileName), "procfile:\n  web: ./server\n")
	writeTestFile(t, filepath.Join(realApp, config.LocalFileName), "procfile:\n  web: ./local-server\n")
	writeTestFile(t, filepath.Join(appsDir, "plain", config.FileName), "procfile:\n  worker: ./jobs\n")
	writeTestFile(t, filepath.Join(appsDir, ".hidden", config.FileName), "procfile:\n  web: ./server\n")
	writeTestFile(t, filepath.Join(appsDir, "stray.txt"), "")
	if err := os.Symlink(realApp, filepath.Join(appsDir, "linked")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Apps = appsDir
	found, invalid, err := Discover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].Name != "linked" || found[1].Name != "plain" {
		t.Fatalf("unexpected apps: %+v", found)
	}
	if found[0].Dir != filepath.Join(appsDir, "linked") || found[0].Commands["web"].Line != "./local-server" {
		t.Fatalf("symlinked app should keep the link path and use the local file: %+v", found[0])
	}
	if len(invalid) != 1 || invalid[0].(ScanError).Name != "stray.txt" {
		t.Fatalf("expected stray.txt to be reported, got %v", invalid)
	}
}

func TestDiscoverSingleModeRereadsRootFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	writeTestFile(t, path, "procfile:\n  web: ./server\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "procfile:\n  web: ./server\n  worker: ./jobs\n")
	found, invalid, err := Discover(cfg)
	if err != nil || len(invalid) != 0 {
		t.Fatalf("discover: %v invalid=%v", err, invalid)
	}
	if len(found) != 1 || found[0].Name != filepath.Base(dir) || found[0].Dir != dir || len(found[0].Commands) != 2 {
		t.Fatalf("unexpected single app: %+v", found)
	}
}

func TestGitBranch(t *testing.T) {
	checkout := t.TempDir()
	writeTestFile(t, filepath.Join(checkout, ".env"), "GIT_BRANCH=ignored\n")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(checkout, ".git", "HEAD"), "ref: refs/heads/feature/x\n")

	worktree := t.TempDir()
	writeTestFile(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(checkout, ".git")+"\n")

	detached := t.TempDir()
	if err := os.MkdirAll(filepath.Join(detached, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(detached, ".git", "HEAD"), "4f2a9c0d\n")

	for _, tc := range []struct {
		name, dir string
		env       map[string]string
		want      string
	}{
		{"checkout wins over env", checkout, map[string]string{"GIT_BRANCH": "ignored"}, "feature/x"},
		{"worktree .git file", worktree, nil, "feature/x"},
		{"detached head", detached, nil, ""},
		{"packed release reads env", t.TempDir(), map[string]string{"GIT_BRANCH": " main "}, "main"},
		{"nothing known", t.TempDir(), nil, ""},
	} {
		if got := gitBranch(tc.dir, tc.env); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverFindsAppFileUnderConfig(t *testing.T) {
	root := t.TempDir()
	appsDir := filepath.Join(root, "apps")
	nested := filepath.Join(appsDir, "rails", config.ConfigDir)
	both := filepath.Join(appsDir, "both")
	for _, dir := range []string{nested, filepath.Join(both, config.ConfigDir)} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(nested, config.FileName), "procfile:\n  web: ./server\n")
	writeTestFile(t, filepath.Join(both, config.FileName), "procfile:\n  web: ./server\n")
	writeTestFile(t, filepath.Join(both, config.ConfigDir, config.FileName), "procfile:\n  web: ./server\n")
	cfg := config.Default()
	cfg.Apps = appsDir
	found, invalid, err := Discover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "rails" || found[0].Dir != filepath.Join(appsDir, "rails") {
		t.Fatalf("found = %+v", found)
	}
	if len(invalid) != 1 || !strings.Contains(invalid[0].Error(), "keep one") {
		t.Fatalf("invalid = %v, want the both-folders error", invalid)
	}
}
