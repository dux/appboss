package alerts

import (
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/logstore"
	"dboss/internal/notify"
	"dboss/internal/supervisor"
	"dboss/internal/sysinfo"
)

type fakeApps struct {
	snapshots []supervisor.Snapshot
	host      config.Config
}

func (f fakeApps) Snapshots() []supervisor.Snapshot { return f.snapshots }
func (f fakeApps) HostConfig() config.Config        { return f.host }

type fakeDisks []sysinfo.Dir

func (f fakeDisks) Snapshot() sysinfo.Snapshot { return sysinfo.Snapshot{Dirs: f} }

type fakeStore struct {
	windows map[string]logstore.Window
	since   map[string]time.Time
}

func (f *fakeStore) Window(app string, since time.Time) (logstore.Window, error) {
	f.since[app] = since
	return f.windows[app], nil
}

type recordingSink struct{ events []notify.Event }

func (r *recordingSink) Send(event notify.Event) { r.events = append(r.events, event) }

func snapshot(name string, alerts config.Alerts) supervisor.Snapshot {
	return supervisor.Snapshot{Name: name, LogRetention: time.Hour, Web: config.Web{Alerts: alerts}}
}

func TestRunOnceFiresOverThreshold(t *testing.T) {
	checks := config.Alerts{ErrorRate: 10, SlowP95: config.Duration(2 * time.Second)}
	quiet := snapshot("unlogged", checks)
	quiet.LogRetention = 0
	apps := fakeApps{snapshots: []supervisor.Snapshot{
		snapshot("shop", checks),
		snapshot("calm", checks),
		snapshot("tiny", checks),
		snapshot("off", config.Alerts{}),
		quiet,
	}}
	bad := logstore.Window{Count: 250, Errors: 31, P95: 3200}
	store := &fakeStore{since: map[string]time.Time{}, windows: map[string]logstore.Window{
		"shop":     bad,
		"calm":     {Count: 250, Errors: 2, P95: 120},
		"tiny":     {Count: 5, Errors: 5, P95: 9000},
		"off":      bad,
		"unlogged": bad,
	}}
	sink := &recordingSink{}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	New(apps, store, nil, sink).runOnce(now)

	if len(sink.events) != 2 {
		t.Fatalf("events = %+v, want error-rate and slow for shop only", sink.events)
	}
	rate, slow := sink.events[0], sink.events[1]
	if rate.Type != "error-rate" || rate.App != "shop" || rate.Error != "12.4% 5xx (31 of 250) in 5m" || !rate.Time.Equal(now) {
		t.Fatalf("unexpected error-rate event: %+v", rate)
	}
	if slow.Type != "slow" || slow.App != "shop" || slow.Error != "p95 3.2s over 2s (250 requests in 5m)" {
		t.Fatalf("unexpected slow event: %+v", slow)
	}
	if got := store.since["shop"]; !got.Equal(now.Add(-5 * time.Minute)) {
		t.Fatalf("window start = %v", got)
	}
	for _, skipped := range []string{"off", "unlogged"} {
		if _, read := store.since[skipped]; read {
			t.Fatalf("%s must not be read at all", skipped)
		}
	}
}

func TestDiskLowNamesEachFullFilesystemOnce(t *testing.T) {
	apps := fakeApps{host: config.Config{DiskAlert: 90}}
	disks := fakeDisks{
		{Name: "config", Path: "/srv/dboss", TotalBytes: 100 << 30, FreeBytes: 5 << 30, Percent: 95, Device: 1},
		{Name: "dir/log", Path: "/srv/dboss/.dboss/log", TotalBytes: 100 << 30, FreeBytes: 5 << 30, Percent: 95, Device: 1},
		{Name: "apps", Path: "/data/apps", TotalBytes: 100 << 30, FreeBytes: 50 << 30, Percent: 50, Device: 2},
		{Name: "dir", Path: "/gone", Error: "no such file"},
	}
	sink := &recordingSink{}
	New(apps, &fakeStore{since: map[string]time.Time{}}, disks, sink).runOnce(time.Now())
	if len(sink.events) != 1 {
		t.Fatalf("events = %+v, want one disk-low", sink.events)
	}
	event := sink.events[0]
	if event.Type != "disk-low" || event.App != "" || event.Error != "/srv/dboss 95.0% used, 5.0G free (config, dir/log)" {
		t.Fatalf("unexpected disk-low event: %+v", event)
	}

	apps.host.DiskAlert = 0
	sink.events = nil
	New(apps, &fakeStore{since: map[string]time.Time{}}, disks, sink).runOnce(time.Now())
	if len(sink.events) != 0 {
		t.Fatalf("disk_alert 0 still sent %+v", sink.events)
	}
}
