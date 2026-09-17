package super

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"deploy-boss/internal/config"
	"deploy-boss/internal/ports"
)

func TestHostMatch(t *testing.T) {
	for _, test := range []struct {
		host, pattern string
		match         bool
	}{{"app.test", "app.test", true}, {"a.dev.test", "*.dev.test", true}, {"dev.test", "*.dev.test", false}} {
		if _, got := hostMatch(test.host, test.pattern); got != test.match {
			t.Errorf("hostMatch(%q, %q) = %v", test.host, test.pattern, got)
		}
	}
}

func TestBackoffCaps(t *testing.T) {
	values := []any{"1s", 2.0, "5s"}
	if got := backoff(values, 1); got != time.Second {
		t.Fatalf("first = %s", got)
	}
	if got := backoff(values, 10); got != 5*time.Second {
		t.Fatalf("cap = %s", got)
	}
}

func TestHealthcheckReportsHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/up" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	_, portValue, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portValue)
	if err != nil {
		t.Fatal(err)
	}
	ok, checkErr := healthCheck("http:/up", port, time.Second)
	if ok || checkErr == nil || checkErr.Error() != "Healthcheck on /up returned 403" {
		t.Fatalf("healthcheck = %v, %v", ok, checkErr)
	}
}

func TestSupervisorStartsAndStopsWebProcess(t *testing.T) {
	cfg := supervisorTestConfig(t, [2]int{32100, 32120})
	allocator, err := ports.Open(cfg.StateDir, cfg.Ports.Range, false)
	if err != nil {
		t.Fatal(err)
	}
	manager, invalid, err := New(cfg, allocator)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(invalid) != 0 {
		t.Fatalf("invalid apps: %v", invalid)
	}
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Running)
	if err := manager.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := manager.Snapshot("demo")
	if snapshot.State != Stopped || len(snapshot.Processes) != 0 {
		t.Fatalf("app did not stop: %+v", snapshot)
	}
}

func TestSupervisorStopsAndRestartsDesiredProcessAfterManagerRestart(t *testing.T) {
	cfg := supervisorTestConfig(t, [2]int{32300, 32320})
	allocator, err := ports.Open(cfg.StateDir, cfg.Ports.Range, false)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := New(cfg, allocator)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, first, Running)
	snapshot, _ := first.Snapshot("demo")
	pid := snapshot.Processes[0].PID
	first.Close()
	if alive(pid) {
		t.Fatalf("process %d survived manager close", pid)
	}
	secondAllocator, err := ports.Open(cfg.StateDir, cfg.Ports.Range, false)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := New(cfg, secondAllocator)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	waitForSupervisorState(t, second, Running)
	restarted, _ := second.Snapshot("demo")
	if len(restarted.Processes) != 1 || restarted.Processes[0].Adopted || restarted.Processes[0].PID == pid {
		t.Fatalf("process was not restarted: %+v", restarted)
	}
	if err := second.Stop("demo"); err != nil {
		t.Fatal(err)
	}
}

func TestRescanReloadsAppFoldersFromConfig(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one", "two"} {
		appDir := filepath.Join(root, "apps", name)
		if err := os.MkdirAll(appDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(appDir, "deploy-boss.yaml"), []byte("procfile:\n  worker: /usr/bin/true\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "deploy-boss.config.yaml")
	writeConfig := func(apps string) {
		t.Helper()
		contents := "apps: " + apps + "\nstate_dir: ./state\nlog_dir: ./log\nsocket: ./boss.sock\n"
		if err := os.WriteFile(configPath, []byte(contents), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("[./apps/one]")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	allocator, err := ports.Open(cfg.StateDir, cfg.Ports.Range, false)
	if err != nil {
		t.Fatal(err)
	}
	manager, invalid, err := New(cfg, allocator)
	if err != nil || len(invalid) != 0 {
		t.Fatalf("new manager: %v, invalid: %v", err, invalid)
	}
	defer manager.Close()
	writeConfig("[./apps/one, ./apps/two]")
	invalid, err = manager.Rescan()
	if err != nil || len(invalid) != 0 {
		t.Fatalf("rescan: %v, invalid: %v", err, invalid)
	}
	if snapshots := manager.Snapshots(); len(snapshots) != 2 || snapshots[0].Name != "one" || snapshots[1].Name != "two" {
		t.Fatalf("unexpected apps after rescan: %+v", snapshots)
	}
}

func supervisorTestConfig(t *testing.T, portRange [2]int) config.Config {
	t.Helper()
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "demo")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	appConfig := fmt.Sprintf("procfile:\n  web: %s -test.run=TestSupervisorHelperProcess\n", os.Args[0])
	if err := os.WriteFile(filepath.Join(appDir, "deploy-boss.yaml"), []byte(appConfig), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, ".env"), []byte("BOSS_TEST_HELPER=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Apps = []string{appDir}
	cfg.StateDir = filepath.Join(root, "state")
	cfg.LogDir = filepath.Join(root, "log")
	cfg.Socket = filepath.Join(root, "boss.sock")
	cfg.Ports.Range = portRange
	cfg.Ports.CheckBound = false
	cfg.Defaults.HealthInterval = config.Duration(10 * time.Millisecond)
	cfg.Defaults.HealthTimeout = config.Duration(2 * time.Second)
	return cfg
}

func waitForSupervisorState(t *testing.T, manager *Manager, state State) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot, err := manager.Snapshot("demo")
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.State == state {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("app did not reach %s: %+v", state, snapshot)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSupervisorHelperProcess(t *testing.T) {
	if os.Getenv("BOSS_TEST_HELPER") != "1" {
		return
	}
	port, _ := strconv.Atoi(os.Getenv("PORT"))
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		os.Exit(2)
	}
	defer listener.Close()
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			os.Exit(0)
		}
		_ = connection.Close()
	}
}
