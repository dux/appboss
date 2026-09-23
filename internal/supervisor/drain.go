package supervisor

import (
	"sync/atomic"
	"time"
)

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
		time.Sleep(20 * time.Millisecond)
	}
}

// Enter registers one in-flight proxied request and returns the counter Leave expects.
func (m *Manager) Enter(name string) *atomic.Int64 {
	counter := m.traffic(name)
	counter.Add(1)
	return counter
}

// Leave clears one in-flight request.
func (m *Manager) Leave(counter *atomic.Int64) {
	if counter != nil {
		counter.Add(-1)
	}
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
