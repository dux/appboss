package logstore

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A database created before requests carried a country must keep accepting rows after an upgrade.
func TestRequestsTableGainsCountryColumnInPlace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "demo", "dboss.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE requests (ts TEXT NOT NULL, method TEXT NOT NULL, host TEXT NOT NULL, path TEXT NOT NULL, status INTEGER NOT NULL, duration_ms INTEGER NOT NULL, bytes_out INTEGER NOT NULL, ip TEXT NOT NULL, ua TEXT NOT NULL, request_id TEXT NOT NULL DEFAULT '', process TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO requests (ts, method, host, path, status, duration_ms, bytes_out, ip, ua) VALUES (?, 'GET', 'demo.test', '/old', 200, 1, 1, '1.2.3.4', 'curl')`, stamp(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store := New(dir, 5*time.Millisecond, nil, "", time.Hour, 0)
	defer store.Close()
	if err := store.Record("demo", time.Hour, RequestEntry{Time: time.Now(), Method: "GET", Host: "demo.test", Path: "/new", Status: 200, Country: "HR"}); err != nil {
		t.Fatal(err)
	}
	fresh := waitFor(t, func() ([]RequestEntry, error) { return store.SearchRequests("demo", RequestFilter{Query: "/new"}) })
	if len(fresh) != 1 || fresh[0].Country != "HR" {
		t.Fatalf("row written after the upgrade: %+v", fresh)
	}
	old, err := store.SearchRequests("demo", RequestFilter{Query: "/old"})
	if err != nil || len(old) != 1 || old[0].Country != "" {
		t.Fatalf("row written before the upgrade: %v %+v", err, old)
	}
}
