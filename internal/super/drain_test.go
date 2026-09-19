package super

import (
	"strings"
	"testing"
	"time"

	"dboss/internal/ports"
)

func TestStartOrderPutsWebFirst(t *testing.T) {
	cfg := hookConfig(t, [2]int{33040, 33060}, "procfile:\n  alpha: /usr/bin/true\n  web: /usr/bin/true\n  zeta: /usr/bin/true\nautostart: false\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	runtime, err := manager.runtime("demo")
	if err != nil {
		t.Fatal(err)
	}
	order := runtime.startOrder()
	if strings.Join(order, ",") != "web,alpha,zeta" {
		t.Fatalf("start order = %v, want [web alpha zeta]", order)
	}
}

func TestStopDrainsInFlightRequests(t *testing.T) {
	cfg := hookConfig(t, [2]int{33000, 33020}, "procfile:\n  web: /usr/bin/true\nautostart: false\nstop_timeout: 5s\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	counter := manager.Enter("demo")
	done := make(chan error, 1)
	go func() { done <- manager.Stop("demo") }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, err := manager.Snapshot("demo")
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Draining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("app never reported draining")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("Stop returned while a request was still in flight")
	case <-time.After(200 * time.Millisecond):
	}

	manager.Leave(counter)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	snapshot, _ := manager.Snapshot("demo")
	if snapshot.Draining {
		t.Fatal("app stayed draining after it stopped")
	}
}

func TestDrainGivesUpAfterStopTimeout(t *testing.T) {
	cfg := hookConfig(t, [2]int{33020, 33040}, "procfile:\n  web: /usr/bin/true\nautostart: false\nstop_timeout: 150ms\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	manager.Enter("demo") // never left: the drain must time out
	start := time.Now()
	if err := manager.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("Stop gave up after %s, want at least the stop_timeout", elapsed)
	}
}
