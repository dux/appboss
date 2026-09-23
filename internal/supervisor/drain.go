package supervisor

import (
	"sync/atomic"
	"time"
)

// drainPoll is how often a drain re-reads an in-flight counter.
const drainPoll = 20 * time.Millisecond

// drain marks the app as draining so the proxy stops sending new requests, then waits for the
// in-flight ones to finish, bounded by the host stop_timeout. It runs on the caller's goroutine,
// never the app's, so snapshots stay responsive while it waits.
func (m *Manager) drain(runtime *appRuntime, name string) {
	if err := runtime.call(request{kind: requestDrain, on: true}); err != nil {
		return
	}
	timeout := m.cfg.Defaults.StopTimeout.Value()
	if timeout <= 0 {
		return
	}
	counter := m.traffic(name)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if counter.Load() == 0 {
			return
		}
		time.Sleep(drainPoll)
	}
}

// Pick chooses the ready copy of a web process a proxied request goes to and counts the request
// as in flight, on the copy and on the app, until done is called. ok is false when no copy is
// ready.
func (m *Manager) Pick(app, web string) (port int, done func(), ok bool) {
	if m == nil {
		return 0, nil, false
	}
	chosen := m.routes.pick(app, web)
	if chosen == nil {
		return 0, nil, false
	}
	counter := m.traffic(app)
	counter.Add(1)
	return chosen.port, func() {
		chosen.inflight.Add(-1)
		counter.Add(-1)
	}, true
}

func (m *Manager) traffic(name string) *atomic.Int64 {
	m.inflightMu.Lock()
	defer m.inflightMu.Unlock()
	counter := m.inflight[name]
	if counter == nil {
		counter = &atomic.Int64{}
		m.inflight[name] = counter
	}
	return counter
}
