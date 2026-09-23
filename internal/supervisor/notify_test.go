package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dboss/internal/notify"
	"dboss/internal/ports"
	"dboss/internal/res"
)

type recordingSink struct {
	mu     sync.Mutex
	events []notify.Event
}

func (s *recordingSink) Send(event notify.Event) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}

func (s *recordingSink) waitFor(t *testing.T, eventType string) notify.Event {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for _, event := range s.events {
			if event.Type == eventType {
				s.mu.Unlock()
				return event
			}
		}
		s.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("event %q was not emitted", eventType)
	return notify.Event{}
}

func TestCrashEmitsNotification(t *testing.T) {
	fastRestart(t)
	sink := &recordingSink{}
	cfg := hookConfig(t, [2]int{32900, 32920}, "procfile:\n  web:\n    command: /usr/bin/false\n    hosts: [demo.test]\nautostart: true\nrestart: on-failure\nmax_restarts: 1\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil, sink)
	if err != nil {
		t.Fatal(err)
	}
	manager.Boot()
	defer manager.Close()
	if event := sink.waitFor(t, "crash"); event.App != "demo" || event.Error == "" {
		t.Fatalf("event = %+v", event)
	}
}

func TestRestartLoopEmitsNotification(t *testing.T) {
	fastRestart(t)
	sink := &recordingSink{}
	cfg := hookConfig(t, [2]int{32920, 32940}, "procfile:\n  web:\n    command: /usr/bin/false\n    hosts: [demo.test]\nautostart: true\nrestart: on-failure\nmax_restarts: 5\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil, sink)
	if err != nil {
		t.Fatal(err)
	}
	manager.Boot()
	defer manager.Close()
	if event := sink.waitFor(t, "restart-loop"); event.App != "demo" {
		t.Fatalf("event = %+v", event)
	}
}

func TestHookFailureEmitsNotification(t *testing.T) {
	sink := &recordingSink{}
	cfg := hookConfig(t, [2]int{32940, 32960}, "procfile:\n  web: /usr/bin/true\nautostart: false\nhooks:\n  deploy:\n    command: /usr/bin/false\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.RunHook("demo", "deploy"); err != nil {
		t.Fatal(err)
	}
	waitForHookEnd(t, manager, "demo", "deploy")
	if event := sink.waitFor(t, "hook-failed"); event.App != "demo" {
		t.Fatalf("event = %+v", event)
	}
}

func TestDeployHookEmitsNotification(t *testing.T) {
	sink := &recordingSink{}
	cfg := hookConfig(t, [2]int{33100, 33120}, "procfile:\n  web: /bin/sleep 30\nautostart: false\nhooks:\n  deploy:\n    command: /usr/bin/true\n    restart: true\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.RunHook("demo", "deploy"); err != nil {
		t.Fatal(err)
	}
	waitForHookEnd(t, manager, "demo", "deploy")
	if event := sink.waitFor(t, "deploy"); event.App != "demo" {
		t.Fatalf("event = %+v", event)
	}
}

func TestWakeFailureEmitsNotification(t *testing.T) {
	sink := &recordingSink{}
	cfg := hookConfig(t, [2]int{32960, 32980}, "procfile:\n  web: /bin/sleep 30\nautostart: false\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil, sink)
	if err != nil {
		t.Fatal(err)
	}
	manager.Boot()
	defer manager.Close()
	// A missing command only fails inside sh; a missing app folder fails the spawn itself.
	if err := os.RemoveAll(filepath.Join(cfg.Apps, "demo")); err != nil {
		t.Fatal(err)
	}
	manager.Wake("demo")
	if event := sink.waitFor(t, "wake-failed"); event.App != "demo" || event.Error == "" {
		t.Fatalf("event = %+v", event)
	}
}

func TestCronFailureEmitsNotification(t *testing.T) {
	sink := &recordingSink{}
	cfg := cronTestConfig(t, [2]int{33600, 33620}, "cron:\n  tick:\n    schedule: every 1m\n    command: /bin/sh -c 'exit 3'\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.RunCron("demo", "tick"); err != nil {
		t.Fatal(err)
	}
	if event := sink.waitFor(t, "cron-failed"); event.App != "demo" || !strings.Contains(event.Error, "exit code 3") {
		t.Fatalf("event = %+v", event)
	}
}

// oomBackend reports one more OOM kill on every read, so each exit looks like an OOM kill.
type oomBackend struct {
	res.Procgroup
	reads atomic.Int64
}

func (b *oomBackend) Name() string                  { return "cgroup" }
func (b *oomBackend) OOMKills(string, string) int64 { return b.reads.Add(1) }

func TestOOMKillEmitsNotification(t *testing.T) {
	fastRestart(t)
	sink := &recordingSink{}
	cfg := hookConfig(t, [2]int{33620, 33640}, "procfile:\n  web:\n    command: /usr/bin/false\n    hosts: [demo.test]\nautostart: true\nrestart: never\nmemory_max: 64m\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil, sink)
	if err != nil {
		t.Fatal(err)
	}
	manager.apps["demo"].cgroup = &oomBackend{}
	manager.Boot()
	defer manager.Close()
	if event := sink.waitFor(t, "oom"); !strings.Contains(event.Error, "memory_max 64m") {
		t.Fatalf("oom event = %+v", event)
	}
	if event := sink.waitFor(t, "crash"); !strings.Contains(event.Error, "out of memory") {
		t.Fatalf("crash event = %+v", event)
	}
}
