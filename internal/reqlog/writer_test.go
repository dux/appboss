package reqlog

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerFlushesRequests(t *testing.T) {
	dir := t.TempDir()
	manager := New(dir, 10*time.Millisecond)
	if err := manager.Record("demo", time.Hour, 10*time.Millisecond, Entry{Time: time.Now(), Method: "GET", Host: "demo.test", Path: "/", Status: 200, BytesOut: 2, IP: "127.0.0.1"}); err != nil {
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
	if err := db.QueryRow(`SELECT count(*) FROM requests`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("got %d requests", count)
	}
	rates, err := manager.Rates("demo")
	if err != nil {
		t.Fatal(err)
	}
	if rates.LastMinute != 1 || rates.LastHour != 1 || rates.LastDay != 1 {
		t.Fatalf("unexpected rates: %+v", rates)
	}
}
