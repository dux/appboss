package super

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/ports"
)

func TestProcessEnvPriority(t *testing.T) {
	spec := &apps.App{
		Name:    "demo",
		Env:     map[string]string{"A": "daemon", "B": "daemon", "PATH": "/bin"},
		FileEnv: map[string]string{"B": "file", "C": "file"},
	}
	spec.Config.Env = map[string]string{"A": "config", "B": "config", "D": "config"}
	// extra stands in for Process(name).Env: app config env plus a process override.
	extra := map[string]string{"A": "config", "B": "config", "D": "config", "E": "override"}
	values := processEnv(spec, "web", 123, "/run/dboss.sock", extra)
	want := map[string]string{
		"A": "config", "B": "file", "C": "file", "D": "config", "E": "override",
		"PATH": "/bin", "PORT": "123", "APP_NAME": "demo", "PROC_TYPE": "web", "DBOSS_SOCKET": "/run/dboss.sock",
	}
	for key, value := range want {
		if values[key] != value {
			t.Errorf("%s = %q, want %q", key, values[key], value)
		}
	}
	if len(values) != len(want) {
		t.Errorf("unexpected env keys: %v", values)
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

func TestHealthcheckSendsAppHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/up" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Host != "demo.test" {
			w.WriteHeader(http.StatusForbidden)
		}
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
	ok, checkErr := healthCheck("http:/up", port, "", time.Second)
	if ok || checkErr == nil || checkErr.Error() != "healthcheck on /up returned 403" {
		t.Fatalf("healthcheck without host = %v, %v", ok, checkErr)
	}
	if ok, checkErr := healthCheck("http:/up", port, "demo.test", time.Second); !ok || checkErr != nil {
		t.Fatalf("healthcheck with host = %v, %v", ok, checkErr)
	}
}

func TestSupervisorStartsAndStopsWebProcess(t *testing.T) {
	cfg := supervisorTestConfig(t, [2]int{32100, 32120})
	manager, invalid, err := New(cfg, ports.New(cfg.Ports.Range), nil)
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
	if snapshot.State != Stopped || len(snapshot.Processes) != 1 {
		t.Fatalf("app did not stop: %+v", snapshot)
	}
	service := snapshot.Processes[0]
	if service.Name != "web" || service.State != Stopped || service.PID != 0 || service.Port != 32100 {
		t.Fatalf("stopped app should still list its procfile service: %+v", service)
	}
}

func TestSnapshotListsEveryProcfileService(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "demo")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, config.FileName), []byte("procfile:\n  web: /usr/bin/true\n  job: /usr/bin/true\nautostart: false\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Apps = filepath.Join(root, "apps")
	cfg.StateDir = filepath.Join(root, "state")
	cfg.LogDir = filepath.Join(root, "log")
	cfg.Socket = filepath.Join(root, "dboss.sock")
	cfg.Ports.Range = [2]int{32600, 32620}
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	snapshot, err := manager.Snapshot("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Processes) != 2 {
		t.Fatalf("stopped app should list both procfile services: %+v", snapshot.Processes)
	}
	// Processes follow the sorted procfile names, and ports are assigned in the same order.
	if snapshot.Processes[0].Name != "job" || snapshot.Processes[1].Name != "web" {
		t.Fatalf("unexpected service order: %+v", snapshot.Processes)
	}
	for _, service := range snapshot.Processes {
		if service.State != Stopped || service.PID != 0 {
			t.Fatalf("service not stopped: %+v", service)
		}
	}
	if snapshot.Processes[0].Port != 32600 || snapshot.Processes[1].Port != 32601 {
		t.Fatalf("reserved ports missing: %+v", snapshot.Processes)
	}
}

func TestSupervisorStopsAndRestartsDesiredProcessAfterManagerRestart(t *testing.T) {
	cfg := supervisorTestConfig(t, [2]int{32300, 32320})
	first, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
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
	second, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	waitForSupervisorState(t, second, Running)
	restarted, _ := second.Snapshot("demo")
	if len(restarted.Processes) != 1 || restarted.Processes[0].PID == pid || restarted.Processes[0].Port != 32300 {
		t.Fatalf("process was not restarted on the fixed port: %+v", restarted)
	}
	if err := second.Stop("demo"); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorStartsEveryAppWithoutRunningList(t *testing.T) {
	cfg := supervisorTestConfig(t, [2]int{32500, 32520})
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	waitForSupervisorState(t, manager, Running)
	if err := manager.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	manager.Close()
	second, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if snapshot, _ := second.Snapshot("demo"); snapshot.State != Stopped {
		t.Fatalf("stopped app was started again on restart: %+v", snapshot)
	}
}

func TestRestartDoesNotOrphanProcess(t *testing.T) {
	cfg := supervisorTestConfig(t, [2]int{32400, 32420})
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Running)
	seen := map[int]bool{}
	for i := 0; i < 3; i++ {
		if err := manager.Restart("demo"); err != nil {
			t.Fatal(err)
		}
		waitForSupervisorState(t, manager, Running)
		snapshot, _ := manager.Snapshot("demo")
		if len(snapshot.Processes) != 1 || snapshot.Processes[0].Port != 32400 {
			t.Fatalf("restart %d: %+v", i, snapshot)
		}
		seen[snapshot.Processes[0].PID] = true
	}
	// Give any stale exit event a chance to be (wrongly) applied before checking.
	time.Sleep(200 * time.Millisecond)
	snapshot, _ := manager.Snapshot("demo")
	if snapshot.State != Running || len(snapshot.Processes) != 1 {
		t.Fatalf("stale event disturbed the app: %+v", snapshot)
	}
	current := snapshot.Processes[0].PID
	for pid := range seen {
		if pid != current && alive(pid) {
			t.Fatalf("process %d was orphaned", pid)
		}
	}
	if err := manager.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	if alive(current) {
		t.Fatalf("process %d survived stop", current)
	}
}

func TestSpawnKillsSquatterOnPort(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof is not installed")
	}
	cfg := supervisorTestConfig(t, [2]int{32500, 32520})
	squatter := startListenerHelperOnPort(t, 32500)
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Running)
	snapshot, _ := manager.Snapshot("demo")
	if snapshot.Processes[0].Port != 32500 {
		t.Fatalf("app did not take its fixed port: %+v", snapshot)
	}
	if err := squatter.Wait(); err == nil {
		t.Fatal("squatter exited without a signal")
	}
}

func TestPortsFollowAppNameOrder(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"zeta", "alpha"} {
		appDir := filepath.Join(root, "apps", name)
		if err := os.MkdirAll(appDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(appDir, config.FileName), []byte("procfile:\n  web: /usr/bin/true\n  worker: /usr/bin/true\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.Apps = filepath.Join(root, "apps")
	cfg.StateDir = filepath.Join(root, "state")
	cfg.LogDir = filepath.Join(root, "log")
	cfg.Socket = filepath.Join(root, "dboss.sock")
	cfg.Ports.Range = [2]int{32600, 32620}
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	want := map[string]int{"alpha/web": 32600, "alpha/worker": 32601, "zeta/web": 32602, "zeta/worker": 32603}
	got := manager.Ports()
	for key, port := range want {
		if got[key] != port {
			t.Fatalf("ports = %v, want %v", got, want)
		}
	}
}

func TestRescanPicksUpNewAppsDirectoryEntries(t *testing.T) {
	root := t.TempDir()
	addApp := func(name string) {
		t.Helper()
		appDir := filepath.Join(root, "apps", name)
		if err := os.MkdirAll(appDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(appDir, config.FileName), []byte("procfile:\n  worker: /usr/bin/true\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	addApp("one")
	configPath := filepath.Join(root, config.FileName)
	if err := os.WriteFile(configPath, []byte("apps: ./apps\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	manager, invalid, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil || len(invalid) != 0 {
		t.Fatalf("new manager: %v, invalid: %v", err, invalid)
	}
	defer manager.Close()
	addApp("two")
	invalid, err = manager.Rescan()
	if err != nil || len(invalid) != 0 {
		t.Fatalf("rescan: %v, invalid: %v", err, invalid)
	}
	if snapshots := manager.Snapshots(); len(snapshots) != 2 || snapshots[0].Name != "one" || snapshots[1].Name != "two" {
		t.Fatalf("unexpected apps after rescan: %+v", snapshots)
	}
}

func TestSupervisorSkipsAutostartFalseOnFirstStart(t *testing.T) {
	cfg := supervisorTestConfigApp(t, [2]int{32700, 32720}, "autostart: false\n")
	manager, invalid, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(invalid) != 0 {
		t.Fatalf("invalid apps: %v", invalid)
	}
	snapshot, err := manager.Snapshot("demo")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != Stopped {
		t.Fatalf("autostart: false app started on first start: %+v", snapshot)
	}
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Running)
}

func TestSupervisorSkipsAutostartFalseWhenListedInRunningJSON(t *testing.T) {
	cfg := supervisorTestConfigApp(t, [2]int{32800, 32820}, "autostart: false\n")
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "running.json"), []byte("[\n  \"demo\"\n]\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	manager, invalid, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(invalid) != 0 {
		t.Fatalf("invalid apps: %v", invalid)
	}
	snapshot, err := manager.Snapshot("demo")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != Stopped {
		t.Fatalf("autostart: false app started from running.json: %+v", snapshot)
	}
}

func supervisorTestConfig(t *testing.T, portRange [2]int) config.Config {
	return supervisorTestConfigApp(t, portRange, "")
}

func supervisorTestConfigApp(t *testing.T, portRange [2]int, extraYAML string) config.Config {
	t.Helper()
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "demo")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	appConfig := fmt.Sprintf("procfile:\n  web: %s -test.run=TestSupervisorHelperProcess\n%s", os.Args[0], extraYAML)
	if err := os.WriteFile(filepath.Join(appDir, config.FileName), []byte(appConfig), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, ".env"), []byte("dboss_TEST_HELPER=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Apps = filepath.Join(root, "apps")
	cfg.StateDir = filepath.Join(root, "state")
	cfg.LogDir = filepath.Join(root, "log")
	cfg.Socket = filepath.Join(root, "dboss.sock")
	cfg.Ports.Range = portRange
	cfg.Defaults.StopTimeout = config.Duration(2 * time.Second)
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
	if os.Getenv("dboss_TEST_HELPER") != "1" {
		return
	}
	port, _ := strconv.Atoi(os.Getenv("PORT"))
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		os.Exit(2)
	}
	defer listener.Close()
	// First incarnation of an app started with HANG_ONCE passes readiness, then drops its
	// listener while staying alive, so only the liveness check can notice it. The restarted
	// incarnation finds the marker and serves normally.
	if marker := os.Getenv("dboss_TEST_HELPER_HANG_ONCE"); marker != "" {
		if _, statErr := os.Stat(marker); errors.Is(statErr, os.ErrNotExist) {
			_ = os.WriteFile(marker, []byte("1"), 0o640)
			go func() {
				time.Sleep(200 * time.Millisecond)
				_ = listener.Close()
			}()
			select {}
		}
	}
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			os.Exit(0)
		}
		_ = connection.Close()
	}
}

func TestSupervisorRestartsUnhealthyWebProcess(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "hang-once")
	extra := fmt.Sprintf("env:\n  dboss_TEST_HELPER_HANG_ONCE: %s\nunhealthy_threshold: 2\nrestart_backoff: [10ms, 1.0, 50ms]\n", marker)
	cfg := supervisorTestConfigApp(t, [2]int{32200, 32220}, extra)
	manager, invalid, err := New(cfg, ports.New(cfg.Ports.Range), nil)
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := manager.Snapshot("demo")
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.State == Running && len(snapshot.Processes) == 1 && snapshot.Processes[0].Restarts >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("web process was not restarted after going unhealthy: %+v", snapshot)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRescanReloadsDefaultsAndReportsHostKeys(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "one")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, config.FileName), []byte("procfile:\n  web: /usr/bin/true\nhosts: [one.test]\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, config.FileName)
	if err := os.WriteFile(configPath, []byte("apps: ./apps\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := os.WriteFile(configPath, []byte("apps: ./apps\nproxy:\n  listen: 127.0.0.1:9999\ndefaults:\n  static: ./public\n  headers:\n    X-Robots-Tag: none\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rescan(); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := manager.Snapshot("one")
	if snapshot.Web.Static != "./public" || snapshot.Web.Headers["X-Robots-Tag"] != "none" || snapshot.Dir != appDir {
		t.Fatalf("defaults did not reach the app: %+v", snapshot)
	}
	if keys := manager.RestartRequired(); len(keys) != 1 || keys[0] != "proxy" {
		t.Fatalf("restart required = %v", keys)
	}
	if err := manager.SetMaintenance("one", true); err != nil {
		t.Fatal(err)
	}
	if snapshot, _ = manager.Snapshot("one"); !snapshot.Maintenance {
		t.Fatal("maintenance flag not set")
	}
	manager.Close()
	second, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if snapshot, _ = second.Snapshot("one"); !snapshot.Maintenance {
		t.Fatal("maintenance flag did not survive a restart")
	}
	if err := second.SetMaintenance("one", false); err != nil {
		t.Fatal(err)
	}
	if snapshot, _ = second.Snapshot("one"); snapshot.Maintenance {
		t.Fatal("maintenance flag not cleared")
	}
}

func TestIdleStopKeepsAppWithInFlightRequest(t *testing.T) {
	cfg := supervisorTestConfigApp(t, [2]int{33300, 33320}, "idle_stop: 150ms\n")
	cfg.Daemon.IdleTick = config.Duration(20 * time.Millisecond)
	manager, invalid, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(invalid) != 0 {
		t.Fatalf("invalid apps: %v", invalid)
	}
	waitForSupervisorState(t, manager, Running)

	runtime, err := manager.runtime("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.call(request{kind: requestTouch}); err != nil {
		t.Fatal(err)
	}

	// An open request, a websocket being the long-lived case, must outlast idle_stop.
	counter := manager.Enter("demo")
	time.Sleep(500 * time.Millisecond)
	if snapshot, _ := manager.Snapshot("demo"); snapshot.State != Running {
		t.Fatalf("idle_stop stopped an app with an in-flight request: %+v", snapshot)
	}

	manager.Leave(counter)
	waitForSupervisorState(t, manager, Stopped)
}
