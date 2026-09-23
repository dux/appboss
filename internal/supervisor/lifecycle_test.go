package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dboss/internal/ports"
)

// lifecycleManager runs the helper web app with shell steps that append their name to a trace file.
func lifecycleManager(t *testing.T, portRange [2]int, extra string) (*Manager, string, string) {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "trace")
	cfg := supervisorTestConfigApp(t, portRange, strings.ReplaceAll(extra, "TRACE", trace))
	manager, invalid, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil || len(invalid) != 0 {
		t.Fatalf("new manager: %v, invalid: %v", err, invalid)
	}
	t.Cleanup(manager.Close)
	return manager, trace, cfg.StateDir
}

func readTrace(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestLifecycleCreateOnceStartEveryTime(t *testing.T) {
	manager, trace, stateDir := lifecycleManager(t, [2]int{33400, 33420}, "autostart: false\nlifecycle:\n  create: echo create >> TRACE\n  start: echo start >> TRACE\n")
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Running)
	if got := readTrace(t, trace); got != "create\nstart" {
		t.Fatalf("trace after start = %q", got)
	}
	if err := manager.Restart("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Running)
	if got := readTrace(t, trace); got != "create\nstart\nstart" {
		t.Fatalf("trace after restart = %q", got)
	}
	created, err := loadNames(filepath.Join(stateDir, "created.json"))
	if err != nil || !created["demo"] {
		t.Fatalf("created.json = %v, %v", created, err)
	}
}

func TestLifecycleFailedCreateCrashesAndRetries(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "tried")
	steps := fmt.Sprintf("autostart: false\nlifecycle:\n  create: test -f %[1]s || { touch %[1]s; exit 3; }\n  start: echo start >> TRACE\n", marker)
	manager, trace, stateDir := lifecycleManager(t, [2]int{33420, 33440}, steps)
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Crashed)
	snapshot, _ := manager.Snapshot("demo")
	if !strings.Contains(snapshot.Error, "lifecycle create") {
		t.Fatalf("error = %q", snapshot.Error)
	}
	for _, process := range snapshot.Processes {
		if process.PID != 0 {
			t.Fatalf("%s was spawned after a failed create", process.Name)
		}
	}
	if got := readTrace(t, trace); got != "" {
		t.Fatalf("start ran after a failed create: %q", got)
	}
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Running)
	created, _ := loadNames(filepath.Join(stateDir, "created.json"))
	if !created["demo"] {
		t.Fatal("a create that succeeded on retry was not recorded")
	}
}

func TestLifecycleStopKillsARunningStep(t *testing.T) {
	manager, _, _ := lifecycleManager(t, [2]int{33440, 33460}, "autostart: false\nlifecycle:\n  start: sleep 30\n")
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Starting)
	began := time.Now()
	if err := manager.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Stopped)
	if elapsed := time.Since(began); elapsed > 3*time.Second {
		t.Fatalf("stop waited %s for the start step", elapsed)
	}
}

func TestLifecycleStartIsNotRerunForACrashedProcess(t *testing.T) {
	fastRestart(t)
	marker := filepath.Join(t.TempDir(), "hang-once")
	extra := fmt.Sprintf("env:\n  dboss_TEST_HELPER_HANG_ONCE: %s\nunhealthy_threshold: 2\nlifecycle:\n  start: echo start >> TRACE\n", marker)
	manager, trace, _ := lifecycleManager(t, [2]int{33460, 33480}, "autostart: false\nliveness_interval: 50ms\n"+extra)
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Running)
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, _ := manager.Snapshot("demo")
		if snapshot.State == Running && len(snapshot.Processes) == 1 && snapshot.Processes[0].Restarts > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("web process was never restarted: %+v", snapshot)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := readTrace(t, trace); got != "start" {
		t.Fatalf("trace = %q, want one start", got)
	}
}

func TestLifecycleDestroyRunsAfterStopWithTheFolder(t *testing.T) {
	manager, trace, stateDir := lifecycleManager(t, [2]int{33480, 33500}, "autostart: false\ndeletable: true\nlifecycle:\n  create: echo create >> TRACE\n  destroy: test -f dboss.yaml && echo destroy >> TRACE\n")
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Running)
	if err := manager.Destroy("demo"); err != nil {
		t.Fatal(err)
	}
	if got := readTrace(t, trace); got != "create\ndestroy" {
		t.Fatalf("trace = %q", got)
	}
	created, _ := loadNames(filepath.Join(stateDir, "created.json"))
	if created["demo"] {
		t.Fatal("destroy left the app in created.json")
	}
}

func TestLifecycleFailedDestroyStillDestroys(t *testing.T) {
	manager, _, _ := lifecycleManager(t, [2]int{33500, 33520}, "autostart: false\ndeletable: true\nlifecycle:\n  destroy: exit 4\n")
	if err := manager.Destroy("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Snapshot("demo"); err == nil {
		t.Fatal("app survived a failed destroy step")
	}
}
