package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/ports"
)

// hookConfig writes a full app config so a test can pick its own procfile command.
func hookConfig(t *testing.T, portRange [2]int, appYAML string) config.Config {
	t.Helper()
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "demo")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, config.FileName), []byte(appYAML), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Apps = filepath.Join(root, "apps")
	cfg.StateDir = filepath.Join(root, "state")
	cfg.LogDir = filepath.Join(root, "log")
	cfg.Socket = filepath.Join(root, "dboss.sock")
	cfg.Tokens.Dboss = "hook-token"
	cfg.Management.Host = config.List{"dboss.example.com"}
	cfg.Ports = portRange
	cfg.Defaults.StopTimeout = config.Duration(2 * time.Second)
	cfg.Defaults.HealthTimeout = config.Duration(2 * time.Second)
	return cfg
}

func waitForHookEnd(t *testing.T, manager *Manager, app, hook string) HookSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := manager.Snapshot(app)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range snapshot.Hooks {
			if state.Name == hook && !state.Running && !state.LastEnd.IsZero() {
				return state
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("hook %s did not finish", hook)
	return HookSnapshot{}
}

func TestHookListsRunsAndCarriesTheToken(t *testing.T) {
	cfg := hookConfig(t, [2]int{32800, 32820}, "procfile:\n  web: /usr/bin/true\nautostart: false\nhooks:\n  deploy:\n    command: /bin/echo hello\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	snapshot, err := manager.Snapshot("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Hooks) != 1 || snapshot.Hooks[0].Name != "deploy" {
		t.Fatalf("hook snapshot = %+v", snapshot.Hooks)
	}

	infos, err := manager.Hooks("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].URL != "https://dboss.example.com/hooks/demo/deploy?token=hook-token" {
		t.Fatalf("hook info = %+v", infos)
	}
	token, err := manager.HookToken("demo", "deploy")
	if err != nil || token != "hook-token" {
		t.Fatalf("HookToken = %q, %v", token, err)
	}
	if _, err := manager.HookToken("demo", "missing"); err == nil {
		t.Fatal("an unknown hook must not have a token")
	}

	if err := manager.RunHook("demo", "deploy"); err != nil {
		t.Fatal(err)
	}
	state := waitForHookEnd(t, manager, "demo", "deploy")
	if state.LastExit != 0 || state.LastError != "" {
		t.Fatalf("hook ended badly: %+v", state)
	}
}

func TestHookURLNeedsTheToken(t *testing.T) {
	cfg := hookConfig(t, [2]int{32820, 32840}, "procfile:\n  web: /usr/bin/true\nautostart: false\nhooks:\n  deploy:\n    command: /bin/echo hi\n")
	cfg.Tokens.Dboss = ""
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	infos, _ := manager.Hooks("demo")
	if len(infos) != 1 || infos[0].URL != "" {
		t.Fatalf("a hook without tokens.dboss must have no URL: %+v", infos)
	}
}

func TestHookWithRestartStartsTheApp(t *testing.T) {
	cfg := hookConfig(t, [2]int{32860, 32880}, "procfile:\n  web: /bin/sleep 30\nautostart: false\nhooks:\n  deploy:\n    command: /usr/bin/true\n    restart: true\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	if err := manager.RunHook("demo", "deploy"); err != nil {
		t.Fatal(err)
	}
	waitForHookEnd(t, manager, "demo", "deploy")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, _ := manager.Snapshot("demo")
		started := false
		for _, process := range snapshot.Processes {
			started = started || process.Name == "web" && process.PID != 0
		}
		// The hook reads restarting until the restart has returned, so a started web process
		// and a hook that is done must be seen together eventually.
		if started && !snapshot.Hooks[0].Restarting {
			if snapshot.Hooks[0].LastError != "" {
				t.Fatalf("restart hook error: %s", snapshot.Hooks[0].LastError)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("web process was not started after the restart hook")
}

func TestHookKeepsTheOutputOfItsLastRun(t *testing.T) {
	cfg := hookConfig(t, [2]int{32880, 32900}, "procfile:\n  web: /usr/bin/true\nautostart: false\nhooks:\n  deploy:\n    command: /bin/echo not possible to fast-forward\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.RunHook("demo", "deploy"); err != nil {
		t.Fatal(err)
	}
	waitForHookEnd(t, manager, "demo", "deploy")
	infos, err := manager.Hooks("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Output != "not possible to fast-forward\n" {
		t.Fatalf("hook output = %+v", infos)
	}
}

func TestOutputTailKeepsTheEnd(t *testing.T) {
	var tail outputTail
	_, _ = tail.Write([]byte(strings.Repeat("a", hookOutputTail)))
	_, _ = tail.Write([]byte("end"))
	if got := tail.String(); len(got) != hookOutputTail || !strings.HasSuffix(got, "aend") {
		t.Fatalf("tail = %d bytes ending %q", len(got), got[len(got)-4:])
	}
	tail.Reset()
	if tail.String() != "" {
		t.Fatal("reset kept output")
	}
}
