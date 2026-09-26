package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"dboss/internal/events"
	"dboss/internal/logstore"
)

func TestSeedPopulatesDatabases(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "log")
	now := time.Now()
	if err := seed(dir, []string{"demo"}, now); err != nil {
		t.Fatal(err)
	}
	// A second run must recreate, not add to, what the first wrote.
	if err := seed(dir, []string{"demo"}, now); err != nil {
		t.Fatal(err)
	}

	store := logstore.New(dir, time.Hour, nil, "", time.Hour, 0)
	defer store.Close()
	requests, err := store.SearchRequests("demo", logstore.RequestFilter{Limit: 5})
	if err != nil || len(requests) == 0 {
		t.Fatalf("seeded requests: %v %d", err, len(requests))
	}
	logs, err := store.SearchLogs("demo", logstore.LogFilter{Query: "slow"})
	if err != nil || len(logs) == 0 {
		t.Fatalf("seeded logs: %v %d", err, len(logs))
	}

	exceptions, err := store.Exceptions("demo", logstore.ExceptionFilter{})
	if err != nil || len(exceptions) != 2 {
		t.Fatalf("seeded exceptions: %v %d", err, len(exceptions))
	}
	resolved := 0
	for _, group := range exceptions {
		if group.Count == 0 || len(group.Minutes) != 2 || len(group.Minutes[0].Users) == 0 || len(group.Minutes[0].IPs) == 0 {
			t.Fatalf("seeded exception group = %+v", group)
		}
		if group.IsResolved {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("seeded resolved groups = %d, want 1", resolved)
	}

	eventStore := events.NewStore(dir)
	partitions, err := eventStore.Partitions("demo")
	if err != nil || len(partitions) == 0 {
		t.Fatalf("seeded events: %v %d", err, len(partitions))
	}
	// The seeded funnel's first step and a tag filter must both match: every visitor has a
	// page_view carrying a plan tag.
	filter, err := events.ParseFilter("page_view plan:pro")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := events.NewReader(eventStore).Latest("demo", filter, 10, now)
	if err != nil || len(latest) == 0 {
		t.Fatalf("filtered events: %v %d", err, len(latest))
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, logstore.HostApp, "dboss.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count FROM blocked WHERE path = ?`, "/wp-login.php").Scan(&count); err != nil || count != seedBlocked["/wp-login.php"] {
		t.Fatalf("blocked count = %d, err %v, want %d", count, err, seedBlocked["/wp-login.php"])
	}
	var audit int
	if err := db.QueryRow(`SELECT count(*) FROM audit`).Scan(&audit); err != nil || audit != len(seedAuditRow) {
		t.Fatalf("audit rows = %d, err %v, want %d", audit, err, len(seedAuditRow))
	}
}
