package alerts

import (
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/logstore"
	"dboss/internal/notify"
	"dboss/internal/supervisor"
)

type fakeApps struct{ snapshots []supervisor.Snapshot }

func (f fakeApps) Snapshots() []supervisor.Snapshot { return f.snapshots }

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

	New(apps, store, sink).runOnce(now)

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
