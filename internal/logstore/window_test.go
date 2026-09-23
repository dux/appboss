package logstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowSummarizesRequests(t *testing.T) {
	store := New(t.TempDir(), 5*time.Millisecond, nil, "", time.Hour, 0)
	defer store.Close()

	now := time.Now()
	for i := 1; i <= 20; i++ {
		status := 200
		if i > 15 {
			status = 502
		}
		entry := RequestEntry{Time: now, Method: "GET", Host: "demo.test", Path: "/", Status: status, DurationMS: int64(i * 10), BytesOut: 100}
		if err := store.Record("demo", time.Hour, entry); err != nil {
			t.Fatal(err)
		}
	}
	// Outside the window: must not count.
	if err := store.Record("demo", time.Hour, RequestEntry{Time: now.Add(-time.Hour), Method: "GET", Host: "demo.test", Path: "/old", Status: 500, DurationMS: 9000}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() ([]RequestEntry, error) {
		rows, err := store.SearchRequests("demo", RequestFilter{})
		if len(rows) < 21 {
			return nil, err
		}
		return rows, err
	})

	window, err := store.Window("demo", now.Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if window.Count != 20 || window.Errors != 5 || window.BytesOut != 2000 {
		t.Fatalf("unexpected totals: %+v", window)
	}
	if window.ErrorRate() != 25 {
		t.Fatalf("error rate = %v, want 25", window.ErrorRate())
	}
	if window.P50 != 100 || window.P95 != 190 || window.P99 != 200 {
		t.Fatalf("unexpected quantiles: %+v", window)
	}

	empty, err := store.Window("demo", now.Add(time.Minute))
	if err != nil || empty != (Window{}) {
		t.Fatalf("empty window: %v %+v", err, empty)
	}
}

func TestReadsNeverCreateADatabase(t *testing.T) {
	dir := t.TempDir()
	store := New(dir, 5*time.Millisecond, nil, "", time.Hour, 0)
	defer store.Close()

	window, err := store.Window("quiet", time.Now().Add(-time.Hour))
	if err != nil || window != (Window{}) {
		t.Fatalf("app without a database: %v %+v", err, window)
	}
	// Every read path, not just Window: the console polls rates for apps that may never log.
	if rates, err := store.Rates("quiet"); err != nil || rates != (Rates{}) {
		t.Fatalf("Rates: %v %+v", err, rates)
	}
	if _, err := store.SearchLogs("quiet", LogFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SearchRequests("quiet", RequestFilter{}); err != nil {
		t.Fatal(err)
	}
	if offsets, err := store.TailOffsets("quiet"); err != nil || offsets == nil {
		t.Fatalf("TailOffsets: %v %v", err, offsets)
	}
	if _, err := store.SearchAudit(AuditFilter{}); err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"quiet", HostApp} {
		if _, err := os.Stat(filepath.Join(dir, app)); !os.IsNotExist(err) {
			t.Fatalf("a read created %s: %v", filepath.Join(dir, app), err)
		}
	}
}
