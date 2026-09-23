package supervisor

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dboss/internal/ports"
)

// rollConfig is supervisorTestConfigApp with keys under the web entry, such as count.
func rollConfig(t *testing.T, portRange [2]int, webYAML, extraYAML string) *Manager {
	t.Helper()
	cfg := supervisorTestConfigApp(t, portRange, extraYAML)
	path := filepath.Join(cfg.Apps, "demo", "dboss.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(data), "    hosts: [demo.test]\n", "    hosts: [demo.test]\n"+webYAML, 1)
	if err := os.WriteFile(path, []byte(patched), 0o640); err != nil {
		t.Fatal(err)
	}
	manager, invalid, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil || len(invalid) > 0 {
		t.Fatalf("new: %v %v", err, invalid)
	}
	t.Cleanup(manager.Close)
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Running)
	return manager
}

func livePIDs(t *testing.T, manager *Manager) map[string]int {
	t.Helper()
	snapshot, err := manager.Snapshot("demo")
	if err != nil {
		t.Fatal(err)
	}
	pids := map[string]int{}
	for _, process := range snapshot.Processes {
		if process.PID != 0 {
			pids[process.Name] = process.PID
		}
	}
	return pids
}

func TestRollingRestartNeverDropsTheRoute(t *testing.T) {
	manager := rollConfig(t, [2]int{33700, 33720}, "", "")
	before := livePIDs(t, manager)

	var failures, picks atomic.Int64
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-stop:
				return
			default:
			}
			port, done, ok := manager.Pick("demo", "web")
			if !ok {
				failures.Add(1)
				continue
			}
			picks.Add(1)
			// Paced so the loop cannot run the box out of ephemeral ports for the next test.
			time.Sleep(time.Millisecond)
			connection, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
			if err != nil {
				failures.Add(1)
			} else {
				_ = connection.Close()
			}
			done()
		}
	}()
	for range 2 {
		if err := manager.Restart("demo"); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	<-finished

	if failures.Load() > 0 || picks.Load() == 0 {
		t.Fatalf("%d of %d requests failed during the restart", failures.Load(), picks.Load()+failures.Load())
	}
	after := livePIDs(t, manager)
	if len(after) != 1 || after["web"] == before["web"] {
		t.Fatalf("web was not replaced: before %v, after %v", before, after)
	}
	waitFor(t, func() bool { return !alive(before["web"]) }, "old copy still running")
	if snapshot, _ := manager.Snapshot("demo"); snapshot.State != Running || snapshot.Rolling {
		t.Fatalf("app after roll: %+v", snapshot)
	}
}

func TestRollingRestartKeepsOldCopyWhenNewOneFails(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	manager := rollConfig(t, [2]int{33720, 33740}, "", fmt.Sprintf("env:\n  dboss_TEST_HELPER_FAIL_AGAIN: %s\n", marker))
	before := livePIDs(t, manager)

	err := manager.Restart("demo")
	if err == nil || !strings.Contains(err.Error(), "exited with code 3") {
		t.Fatalf("restart error = %v, want the new copy's exit", err)
	}
	snapshot, _ := manager.Snapshot("demo")
	if snapshot.State != Running || !strings.Contains(snapshot.Error, "previous version keeps serving") {
		t.Fatalf("app after aborted roll: %+v", snapshot)
	}
	if after := livePIDs(t, manager); after["web"] != before["web"] || !alive(before["web"]) {
		t.Fatalf("old copy was replaced: before %v, after %v", before, after)
	}
	if _, _, ok := manager.Pick("demo", "web"); !ok {
		t.Fatal("route to the old copy was lost")
	}
}

func TestCountRunsCopiesBehindOneRoute(t *testing.T) {
	manager := rollConfig(t, [2]int{33740, 33760}, "    count: 3\n", "")
	waitFor(t, func() bool {
		snapshot, _ := manager.Snapshot("demo")
		return snapshot.State == Running && len(readyPorts(manager)) == 3
	}, "three ready copies")

	snapshot, _ := manager.Snapshot("demo")
	var names []string
	for _, process := range snapshot.Processes {
		names = append(names, process.Name)
		if process.Type != "web" || process.PID == 0 {
			t.Fatalf("process row = %+v", process)
		}
	}
	if strings.Join(names, ",") != "web.1,web.2,web.3" {
		t.Fatalf("instances = %v", names)
	}

	// Least in flight: three held requests land on three different copies.
	seen := map[int]bool{}
	var dones []func()
	for range 3 {
		port, done, ok := manager.Pick("demo", "web")
		if !ok {
			t.Fatal("no route")
		}
		seen[port] = true
		dones = append(dones, done)
	}
	for _, done := range dones {
		done()
	}
	if len(seen) != 3 {
		t.Fatalf("picks went to %v, want three copies", seen)
	}

	// One copy dying leaves the app running on the other two.
	if err := manager.StopProcess("demo", "web.2"); err != nil {
		t.Fatal(err)
	}
	if snapshot, _ := manager.Snapshot("demo"); snapshot.State != Running || len(readyPorts(manager)) != 2 {
		t.Fatalf("after stopping web.2: %+v", snapshot)
	}

	// A restart brings the held copy back and replaces every copy.
	before := livePIDs(t, manager)
	if err := manager.Restart("demo"); err != nil {
		t.Fatal(err)
	}
	after := livePIDs(t, manager)
	if len(after) != 3 {
		t.Fatalf("after restart: %v", after)
	}
	for name, pid := range before {
		if after[name] == pid {
			t.Fatalf("%s kept pid %d through the restart", name, pid)
		}
	}
}

func TestInstanceEnvironment(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "env")
	cfg := hookConfig(t, [2]int{33760, 33780}, fmt.Sprintf("procfile:\n  worker:\n    command: sh -c 'echo $PROC_TYPE $PROC_INSTANCE >> %s; sleep 30'\n    count: 2\n", out))
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		data, _ := os.ReadFile(out)
		return strings.Count(string(data), "\n") == 2
	}, "both workers wrote their environment")
	data, _ := os.ReadFile(out)
	if lines := string(data); !strings.Contains(lines, "worker 1\n") || !strings.Contains(lines, "worker 2\n") {
		t.Fatalf("worker env = %q", lines)
	}
}

// readyPorts lists the routed ports of the demo web process.
func readyPorts(manager *Manager) []int {
	manager.routes.mu.RLock()
	defer manager.routes.mu.RUnlock()
	set := manager.routes.sets[routeKey("demo", "web")]
	if set == nil {
		return nil
	}
	var result []int
	for _, u := range set.upstreams {
		result = append(result, u.port)
	}
	return result
}

func waitFor(t *testing.T, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStopReapsCopiesStillDraining(t *testing.T) {
	manager := rollConfig(t, [2]int{33780, 33800}, "    count: 2\n", "stop_timeout: 30s\n")
	waitFor(t, func() bool { return len(readyPorts(manager)) == 2 }, "two ready copies")
	before := livePIDs(t, manager)

	// A request held open keeps its copy draining long after the roll has moved on.
	_, done, ok := manager.Pick("demo", "web")
	if !ok {
		t.Fatal("no route")
	}
	if err := manager.Restart("demo"); err != nil {
		t.Fatal(err)
	}
	draining := 0
	for _, pid := range before {
		if alive(pid) {
			draining++
		}
	}
	if draining != 1 {
		t.Fatalf("%d old copies still up after the roll, want the one with the open request", draining)
	}
	// Stop gives up on the drain after the host stop_timeout and must still reap the copy.
	if err := manager.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	done()
	for name, pid := range before {
		if alive(pid) {
			t.Fatalf("%s (pid %d) outlived the stop", name, pid)
		}
	}
}
