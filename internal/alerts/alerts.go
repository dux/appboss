// Package alerts watches every app's request log and posts error-rate and slow events to the
// operator webhook. It keeps no state of its own: the thresholds ride the app snapshot and the
// notifier's quiet period bounds the repeats.
package alerts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dboss/internal/logstore"
	"dboss/internal/logx"
	"dboss/internal/notify"
	"dboss/internal/super"
)

// interval is how often the windows are evaluated. The window itself is per app.
const interval = time.Minute

// Snapshotter lists the apps to check.
type Snapshotter interface {
	Snapshots() []super.Snapshot
}

// Store reads the request summary of one app. logstore.Store is the production one.
type Store interface {
	Window(app string, since time.Time) (logstore.Window, error)
}

// Module evaluates the alerts block of every app on a timer.
type Module struct {
	apps   Snapshotter
	store  Store
	sink   notify.Sink
	cancel context.CancelFunc
	done   chan struct{}
}

func New(apps Snapshotter, store Store, sink notify.Sink) *Module {
	return &Module{apps: apps, store: store, sink: sink, done: make(chan struct{})}
}

func (m *Module) Name() string { return "alerts" }

func (m *Module) Start(ctx context.Context) error {
	ctx, m.cancel = context.WithCancel(ctx)
	go m.loop(ctx)
	return nil
}

func (m *Module) Close() error {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
	return nil
}

func (m *Module) loop(ctx context.Context) {
	defer close(m.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.runOnce(now)
		}
	}
}

// runOnce checks every app that keeps a request log and has a check switched on.
func (m *Module) runOnce(now time.Time) {
	for _, snapshot := range m.apps.Snapshots() {
		alerts := snapshot.Web.Alerts
		if snapshot.LogRetention <= 0 || !alerts.Enabled() {
			continue
		}
		window, err := m.store.Window(snapshot.Name, now.Add(-alerts.Window.Value()))
		if err != nil {
			logx.Warnf("alerts %s: %v", snapshot.Name, err)
			continue
		}
		if window.Count == 0 || window.Count < int64(alerts.MinRequests) {
			continue
		}
		span := short(alerts.Window.Value())
		if rate := window.ErrorRate(); alerts.ErrorRate > 0 && rate >= float64(alerts.ErrorRate) {
			m.sink.Send(notify.Event{Type: "error-rate", App: snapshot.Name, Time: now, Error: fmt.Sprintf("%.1f%% 5xx (%d of %d) in %s", rate, window.Errors, window.Count, span)})
		}
		if limit := alerts.SlowP95.Value(); limit > 0 && window.P95 >= float64(limit.Milliseconds()) {
			p95 := time.Duration(window.P95) * time.Millisecond
			m.sink.Send(notify.Event{Type: "slow", App: snapshot.Name, Time: now, Error: fmt.Sprintf("p95 %s over %s (%d requests in %s)", short(p95), short(limit), window.Count, span)})
		}
	}
}

// short prints 5m, not 5m0s.
func short(d time.Duration) string {
	text := d.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}
