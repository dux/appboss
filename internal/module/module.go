// Package module gives the daemon's long-running features one lifecycle: start everything in
// order, close in reverse. A feature that only needs to run for the session implements Module.
package module

import (
	"context"
	"fmt"
	"time"
)

// Module is a daemon feature. Start begins its work; Close releases it. Both must be safe to
// call once, and Start must return without blocking.
type Module interface {
	Name() string
	Start(ctx context.Context) error
	Close() error
}

type Manager struct {
	modules []Module
}

func NewManager(modules ...Module) *Manager { return &Manager{modules: modules} }

// Start starts each module in order. On failure it closes the ones already started and returns
// the module that failed.
func (m *Manager) Start(ctx context.Context) error {
	for i, real := range m.modules {
		if err := real.Start(ctx); err != nil {
			m.closeFrom(i)
			return fmt.Errorf("module %s: %w", real.Name(), err)
		}
	}
	return nil
}

// Close stops every module in reverse order, returning the first error.
func (m *Manager) Close() error { return m.closeFrom(len(m.modules)) }

func (m *Manager) closeFrom(end int) error {
	var first error
	for i := end - 1; i >= 0; i-- {
		if err := m.modules[i].Close(); err != nil && first == nil {
			first = fmt.Errorf("module %s: %w", m.modules[i].Name(), err)
		}
	}
	return first
}

// Ticker is the loop behind a module that runs one pass on a timer: Run starts it, Close stops
// it and waits for a pass in flight.
type Ticker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Run calls pass every interval until Close. immediate runs one pass first, still off the
// caller's goroutine, so a slow pass never delays the daemon's start.
func (t *Ticker) Run(ctx context.Context, interval time.Duration, immediate bool, pass func(context.Context)) {
	ctx, t.cancel = context.WithCancel(ctx)
	t.done = make(chan struct{})
	go func() {
		defer close(t.done)
		if immediate {
			pass(ctx)
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pass(ctx)
			}
		}
	}()
}

func (t *Ticker) Close() error {
	if t.cancel != nil {
		t.cancel()
		<-t.done
	}
	return nil
}
