package apps

import (
	"os"
	"path/filepath"
	"testing"

	"dboss/internal/config"
)

func TestParseProcfile(t *testing.T) {
	commands, err := ParseProcfile(map[string]config.ProcessSpec{"web": {Command: "./server --port x"}, "worker": {Command: "./jobs"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands["web"].Argv[0] != "./server" {
		t.Fatalf("unexpected commands: %#v", commands)
	}
}

func TestLoadEnv(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/env"
	writeTestFile(t, path, "A=one\nexport B=two\nC=\"three words\"\nD='${A}'\n")
	values, err := LoadEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["C"] != "three words" || values["D"] != "${A}" {
		t.Fatalf("unexpected env: %#v", values)
	}
}

func TestValidateAppAcceptsShorthandHost(t *testing.T) {
	app := config.App{Procfile: map[string]config.ProcessSpec{"web": {Command: "./server"}}, WebProcesses: []config.WebProcess{{Name: "web", Hosts: []string{".demo.test"}}}, Hosts: []string{".demo.test"}}
	if _, err := validateApp(app); err != nil {
		t.Fatalf("shorthand host rejected: %v", err)
	}
	app.Hosts = []string{"demo..test"}
	if _, err := validateApp(app); err == nil {
		t.Fatal("malformed host was accepted")
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

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
