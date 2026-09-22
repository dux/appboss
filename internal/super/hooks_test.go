package super

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
	cfg.Management.URL = "https://dboss.example.com"
	cfg.Management.Host = config.List{"dboss.example.com"}
	cfg.Ports.Range = portRange
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

func TestHookListsRunsAndGeneratesSecret(t *testing.T) {
	cfg := hookConfig(t, [2]int{32800, 32820}, "procfile:\n  web: /usr/bin/true\nautostart: false\nhooks:\n  deploy:\n    command: /bin/echo hello\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil, nil)
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
	if len(infos) != 1 || infos[0].Source != "generated" || len(infos[0].Secret) != 64 {
		t.Fatalf("hook info = %+v", infos)
	}
	if !strings.Contains(infos[0].URL, "?token="+infos[0].Secret) {
		t.Fatalf("hook url = %q", infos[0].URL)
	}
	secret, err := manager.HookSecret("demo", "deploy")
	if err != nil || secret != infos[0].Secret {
		t.Fatalf("HookSecret = %q, %v", secret, err)
	}

	if err := manager.RunHook("demo", "deploy"); err != nil {
		t.Fatal(err)
	}
	state := waitForHookEnd(t, manager, "demo", "deploy")
	if state.LastExit != 0 || state.LastError != "" {
		t.Fatalf("hook ended badly: %+v", state)
	}
}

func TestHookRotateAndConfigSecretWins(t *testing.T) {
	cfg := hookConfig(t, [2]int{32820, 32840}, "procfile:\n  web: /usr/bin/true\nautostart: false\nhooks:\n  deploy:\n    command: /bin/echo hi\n    secret: fromconfig\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	infos, _ := manager.Hooks("demo")
	if len(infos) != 1 || infos[0].Source != "config" || infos[0].Secret != "fromconfig" {
		t.Fatalf("hook info = %+v", infos)
	}
	if _, err := manager.RotateHook("demo", "deploy"); err == nil {
		t.Fatal("rotating a config secret was accepted")
	}
}

func TestHookRotateReplacesGeneratedSecret(t *testing.T) {
	cfg := hookConfig(t, [2]int{32840, 32860}, "procfile:\n  web: /usr/bin/true\nautostart: false\nhooks:\n  deploy:\n    command: /bin/echo hi\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	before, _ := manager.Hooks("demo")
	info, err := manager.RotateHook("demo", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if info.Secret == before[0].Secret {
		t.Fatal("rotate did not change the secret")
	}
	if !strings.Contains(info.URL, info.Secret) {
		t.Fatalf("rotated url = %q", info.URL)
	}
	secret, _ := manager.HookSecret("demo", "deploy")
	if secret != info.Secret {
		t.Fatalf("HookSecret after rotate = %q, want %q", secret, info.Secret)
	}
}

func TestGitAuthEnvCarriesTokenInEnvironment(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin"}
	GitAuthEnv(env, "s3cret")
	if env["GITHUB_TOKEN"] != "s3cret" || env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("env = %v", env)
	}
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "credential.helper" {
		t.Fatalf("git config env = %v", env)
	}
	if strings.Contains(env["GIT_CONFIG_VALUE_0"], "s3cret") {
		t.Fatalf("token leaked into the helper: %q", env["GIT_CONFIG_VALUE_0"])
	}
}

func TestHookWithRestartStartsTheApp(t *testing.T) {
	cfg := hookConfig(t, [2]int{32860, 32880}, "procfile:\n  web: /bin/sleep 30\nautostart: false\nhooks:\n  deploy:\n    command: /usr/bin/true\n    restart: true\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil, nil)
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
		for _, process := range snapshot.Processes {
			if process.Name == "web" && process.PID != 0 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("web process was not started after the restart hook")
}
