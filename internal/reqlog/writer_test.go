package reqlog

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerFlushesRequests(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "demo"), 0o750); err != nil {
		t.Fatal(err)
	}
	// A database from before the request_id column existed must be upgraded on open.
	legacy, err := sql.Open("sqlite", filepath.Join(dir, "demo", "requests.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE requests (ts TEXT NOT NULL, method TEXT NOT NULL, host TEXT NOT NULL, path TEXT NOT NULL, status INTEGER NOT NULL, duration_ms INTEGER NOT NULL, bytes_out INTEGER NOT NULL, ip TEXT NOT NULL, ua TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()
	manager := New(dir, 10*time.Millisecond)
	if err := manager.Record("demo", time.Hour, 10*time.Millisecond, Entry{Time: time.Now(), Method: "GET", Host: "demo.test", Path: "/", Status: 200, BytesOut: 2, IP: "127.0.0.1", RequestID: "abc"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "demo", "requests.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	var requestID string
	if err := db.QueryRow(`SELECT count(*), max(request_id) FROM requests`).Scan(&count, &requestID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || requestID != "abc" {
		t.Fatalf("got %d requests, request_id %q", count, requestID)
	}
	rates, err := manager.Rates("demo")
	if err != nil {
		t.Fatal(err)
	}
	if rates.LastMinute != 1 || rates.LastHour != 1 || rates.LastDay != 1 {
		t.Fatalf("unexpected rates: %+v", rates)
	}
}
