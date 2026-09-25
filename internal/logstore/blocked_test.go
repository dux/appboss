package logstore

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordBlockedCountsByPath(t *testing.T) {
	dir := t.TempDir()
	store := New(dir, 5*time.Millisecond, nil, "", time.Hour, 0)
	defer store.Close()

	if err := store.RecordBlocked("/admin"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := store.RecordBlocked("/x.php"); err != nil {
			t.Fatal(err)
		}
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, HostApp, "dboss.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		var count int
		err := db.QueryRow(`SELECT count FROM blocked WHERE path = ?`, "/x.php").Scan(&count)
		if err == nil && count == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("blocked count for /x.php = %d, err %v", count, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	var admin int
	if err := db.QueryRow(`SELECT count FROM blocked WHERE path = ?`, "/admin").Scan(&admin); err != nil || admin != 1 {
		t.Fatalf("/admin count = %d, err %v", admin, err)
	}
}

func TestBlockedListsCounts(t *testing.T) {
	store := New(t.TempDir(), 5*time.Millisecond, nil, "", time.Hour, 0)
	defer store.Close()

	// A host that never blocked a request has no blocked table yet.
	if stats, err := store.Blocked(); err != nil || len(stats) != 0 {
		t.Fatalf("empty blocked: %v %+v", err, stats)
	}
	for i := 0; i < 3; i++ {
		if err := store.RecordBlocked("/x.php"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordBlocked("/admin"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		stats, err := store.Blocked()
		if err != nil {
			t.Fatal(err)
		}
		if len(stats) == 2 {
			if stats[0].Path != "/x.php" || stats[0].Count != 3 || stats[1].Path != "/admin" || stats[1].Count != 1 {
				t.Fatalf("blocked stats = %+v", stats)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("blocked stats = %+v", stats)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBlockedIsCapped(t *testing.T) {
	store := New(t.TempDir(), 5*time.Millisecond, nil, "", time.Hour, 0)
	defer store.Close()

	for i := 0; i < BlockedLimit+5; i++ {
		if err := store.RecordBlocked(fmt.Sprintf("/p%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		stats, err := store.Blocked()
		if err != nil {
			t.Fatal(err)
		}
		if len(stats) == BlockedLimit {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("blocked stats = %d rows, want %d", len(stats), BlockedLimit)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
