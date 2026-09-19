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

func cronTestConfig(t *testing.T, portRange [2]int, cronYAML string) config.Config {
	t.Helper()
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "demo")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	appConfig := "procfile:\n  web: /usr/bin/true\nautostart: false\n" + cronYAML
	if err := os.WriteFile(filepath.Join(appDir, config.FileName), []byte(appConfig), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Apps = filepath.Join(root, "apps")
	cfg.StateDir = filepath.Join(root, "state")
	cfg.LogDir = filepath.Join(root, "log")
	cfg.Socket = filepath.Join(root, "dboss.sock")
	cfg.Ports.Range = portRange
	cfg.Defaults.StopTimeout = config.Duration(2 * time.Second)
	return cfg
}

func TestCronListsAndRunsManually(t *testing.T) {
	cfg := cronTestConfig(t, [2]int{32700, 32720}, "cron:\n  tick:\n    schedule: every 1m\n    command: /bin/echo hello\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	snapshot, err := manager.Snapshot("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Cron) != 1 || snapshot.Cron[0].Name != "tick" || snapshot.Cron[0].Schedule != "every 1m" || snapshot.Cron[0].Next.IsZero() {
		t.Fatalf("cron snapshot = %+v", snapshot.Cron)
	}
	if err := manager.RunCron("demo", "tick"); err != nil {
		t.Fatal(err)
	}
	state := waitForCronEnd(t, manager, "tick")
	if state.LastExit != 0 || state.LastError != "" {
		t.Fatalf("run ended badly: %+v", state)
	}
	sealed, err := manager.SealLogs("demo")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, path := range sealed {
		if strings.Contains(path, "cron-tick") {
			found = true
		}
	}
	if !found {
		t.Fatalf("cron log was not sealed: %v", sealed)
	}
}

func TestCronSkipsOverlapAndTimesOut(t *testing.T) {
	cfg := cronTestConfig(t, [2]int{32720, 32740}, "cron:\n  slow:\n    schedule: every 1m\n    command: /bin/sleep 30\n    timeout: 1s\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.RunCron("demo", "slow"); err != nil {
		t.Fatal(err)
	}
	if err := manager.RunCron("demo", "slow"); err == nil {
		t.Fatal("a second run was allowed while the first was going")
	}
	state := waitForCronEnd(t, manager, "slow")
	if !strings.Contains(state.LastError, "timed out") {
		t.Fatalf("last error = %q", state.LastError)
	}
}

func TestCronTickFiresDueJob(t *testing.T) {
	cfg := cronTestConfig(t, [2]int{32740, 32760}, "cron:\n  tick:\n    schedule: every 1m\n    command: /bin/echo due\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	runtime := manager.apps["demo"]
	if err := runtime.call(request{kind: requestCron, now: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	waitForCronEnd(t, manager, "tick")
}

func TestRescanAppliesNewCron(t *testing.T) {
	cfg := cronTestConfig(t, [2]int{32760, 32780}, "")
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if snapshot, _ := manager.Snapshot("demo"); len(snapshot.Cron) != 0 {
		t.Fatalf("unexpected cron before rescan: %+v", snapshot.Cron)
	}
	appConfig := "procfile:\n  web: /usr/bin/true\nautostart: false\ncron:\n  tick:\n    schedule: every 5m\n    command: /bin/echo hi\n"
	if err := os.WriteFile(filepath.Join(cfg.Apps, "demo", config.FileName), []byte(appConfig), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rescan(); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := manager.Snapshot("demo")
	if len(snapshot.Cron) != 1 || snapshot.Cron[0].Name != "tick" || snapshot.Cron[0].Schedule != "every 5m" {
		t.Fatalf("rescan did not apply cron: %+v", snapshot.Cron)
	}
}

func waitForCronEnd(t *testing.T, manager *Manager, job string) CronSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot, err := manager.Snapshot("demo")
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range snapshot.Cron {
			if state.Name == job && !state.Running && !state.LastEnd.IsZero() {
				return state
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("cron %s did not finish: %+v", job, snapshot.Cron)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
