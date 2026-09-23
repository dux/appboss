// Package alerts watches every app's request log and the host's disks and posts error-rate, slow
// and disk-low events to the operator webhook. It keeps no state of its own: the thresholds ride
// the app snapshot and the host config, and the notifier's quiet period bounds the repeats.
package alerts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dboss/internal/config"
	"dboss/internal/fsutil"
	"dboss/internal/logstore"
	"dboss/internal/logx"
	"dboss/internal/module"
	"dboss/internal/notify"
	"dboss/internal/supervisor"
	"dboss/internal/sysinfo"
)

// interval is how often the windows are evaluated. The window itself is per app.
const interval = time.Minute

// Snapshotter lists the apps to check and hands out the live host config for disk_alert.
type Snapshotter interface {
	Snapshots() []supervisor.Snapshot
	HostConfig() config.Config
}

// Disks reports the filesystems dboss writes to. sysinfo.Inspector is the production one.
type Disks interface {
	Snapshot() sysinfo.Snapshot
}

// Store reads the request summary of one app. logstore.Store is the production one.
type Store interface {
	Window(app string, since time.Time) (logstore.Window, error)
}

// Module evaluates the alerts block of every app on a timer.
type Module struct {
	apps  Snapshotter
	store Store
	disks Disks
	sink  notify.Sink
	loop  module.Ticker
}

func New(apps Snapshotter, store Store, disks Disks, sink notify.Sink) *Module {
	return &Module{apps: apps, store: store, disks: disks, sink: sink}
}

func (m *Module) Name() string { return "alerts" }

func (m *Module) Start(ctx context.Context) error {
	m.loop.Run(ctx, interval, false, func(context.Context) { m.runOnce(time.Now()) })
	return nil
}

func (m *Module) Close() error { return m.loop.Close() }

// runOnce checks the disks, then every app that keeps a request log and has a check switched on.
func (m *Module) runOnce(now time.Time) {
	m.checkDisks(now)
	for _, snapshot := range m.apps.Snapshots() {
		alerts := snapshot.Web.Alerts
		if snapshot.LogRetention <= 0 || !alerts.Enabled() {
			continue
		}
		window, err := m.store.Window(snapshot.Name, now.Add(-config.AlertWindow))
		if err != nil {
			logx.Warnf("alerts %s: %v", snapshot.Name, err)
			continue
		}
		if window.Count == 0 || window.Count < config.AlertMinRequests {
			continue
		}
		span := short(config.AlertWindow)
		if rate := window.ErrorRate(); alerts.ErrorRate > 0 && rate >= float64(alerts.ErrorRate) {
			m.sink.Send(notify.Event{Type: notify.ErrorRate, App: snapshot.Name, Time: now, Error: fmt.Sprintf("%.1f%% 5xx (%d of %d) in %s", rate, window.Errors, window.Count, span)})
		}
		if limit := alerts.SlowP95.Value(); limit > 0 && window.P95 >= float64(limit.Milliseconds()) {
			p95 := time.Duration(window.P95) * time.Millisecond
			m.sink.Send(notify.Event{Type: notify.Slow, App: snapshot.Name, Time: now, Error: fmt.Sprintf("p95 %s over %s (%d requests in %s)", short(p95), short(limit), window.Count, span)})
		}
	}
}

// checkDisks posts one host-level disk-low listing every filesystem past disk_alert. A filesystem
// shared by several directories is named once, with all of them.
func (m *Module) checkDisks(now time.Time) {
	limit := m.apps.HostConfig().DiskAlert
	if limit <= 0 || m.disks == nil {
		return
	}
	var order []uint64
	full := map[uint64][]sysinfo.Dir{}
	for _, dir := range m.disks.Snapshot().Dirs {
		if dir.Error != "" || dir.TotalBytes == 0 || dir.Percent < float64(limit) {
			continue
		}
		if _, ok := full[dir.Device]; !ok {
			order = append(order, dir.Device)
		}
		full[dir.Device] = append(full[dir.Device], dir)
	}
	if len(order) == 0 {
		return
	}
	parts := make([]string, 0, len(order))
	for _, device := range order {
		dirs := full[device]
		names := make([]string, len(dirs))
		for i, dir := range dirs {
			names[i] = dir.Name
		}
		parts = append(parts, fmt.Sprintf("%s %.1f%% used, %s free (%s)", dirs[0].Path, dirs[0].Percent, fsutil.HumanBytes(dirs[0].FreeBytes), strings.Join(names, ", ")))
	}
	m.sink.Send(notify.Event{Type: notify.DiskLow, Time: now, Error: strings.Join(parts, "; ")})
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
