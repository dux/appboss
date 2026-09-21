package super

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"dboss/internal/config"
)

// A healthy app used to be polled at health_interval for the whole life of the process, which
// meant a /up request twice a second forever. Readiness still polls fast, because it decides how
// long a woken visitor waits, but the ongoing check has to back off to liveness_interval.
func TestMonitorSlowsDownOnceReady(t *testing.T) {
	var hits atomic.Int64
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &appRuntime{ctx: ctx, events: make(chan processEvent, 8)}
	runtime.cfg.Proxy.Upstream.DialTimeout = config.Duration(time.Second)
	proc := &process{name: "web", port: port, done: make(chan struct{})}
	defaults := config.Process{
		Health:             "/up",
		HealthInterval:     config.Duration(5 * time.Millisecond),
		LivenessInterval:   config.Duration(10 * time.Second),
		HealthTimeout:      config.Duration(time.Second),
		UnhealthyThreshold: 3,
	}

	go runtime.monitor(proc, defaults, "")
	select {
	case event := <-runtime.events:
		if event.kind != "ready" {
			t.Fatalf("first event = %q, want ready", event.kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("process never became ready")
	}

	// Readiness is over. At the old behavior the 5ms ticker would add ~40 more hits here; with
	// the liveness interval at 10s there must be none.
	settled := hits.Load()
	time.Sleep(200 * time.Millisecond)
	if extra := hits.Load() - settled; extra > 0 {
		t.Fatalf("%d extra checks after ready; the ticker did not back off to liveness_interval", extra)
	}
	close(proc.done)
}

// A hand-run session is the one with an echo. Polling a process a developer is watching every
// ten seconds only litters their own request log, so the check is relaxed there.
func TestLivenessIntervalRelaxesForAHandRunSession(t *testing.T) {
	defaults := config.Process{LivenessInterval: config.Duration(10 * time.Second)}
	server := &appRuntime{}
	if got := server.livenessInterval(defaults); got != 10*time.Second {
		t.Fatalf("a service keeps its configured cadence, got %s", got)
	}
	dev := &appRuntime{echo: NewEcho(io.Discard)}
	if got := dev.livenessInterval(defaults); got != devLivenessInterval {
		t.Fatalf("dev interval = %s, want %s", got, devLivenessInterval)
	}
	// Raising the cadence is the point; an app that asked for something even quieter keeps it.
	patient := config.Process{LivenessInterval: config.Duration(30 * time.Minute)}
	if got := dev.livenessInterval(patient); got != 30*time.Minute {
		t.Fatalf("dev should never poll more often than asked, got %s", got)
	}
}

// unhealthy_threshold 0 means the check stops at readiness, so the liveness interval is never
// reached and a zero value there cannot reset the ticker.
func TestMonitorStopsAtReadinessWithoutAThreshold(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &appRuntime{ctx: ctx, events: make(chan processEvent, 8)}
	runtime.cfg.Proxy.Upstream.DialTimeout = config.Duration(time.Second)
	proc := &process{name: "web", port: port, done: make(chan struct{})}
	defaults := config.Process{
		Health:             "tcp",
		HealthInterval:     config.Duration(5 * time.Millisecond),
		HealthTimeout:      config.Duration(time.Second),
		UnhealthyThreshold: 0,
	}

	done := make(chan struct{})
	go func() {
		runtime.monitor(proc, defaults, "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor kept running after readiness with unhealthy_threshold: 0")
	}
	close(proc.done)
}
