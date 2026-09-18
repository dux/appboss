package logstore

import (
	"context"
	"testing"
	"time"
)

func TestAuditRecordSearchAndPrune(t *testing.T) {
	store := New(t.TempDir(), 5*time.Millisecond, nil, "", time.Hour, time.Hour)
	defer store.Close()
	now := time.Now()
	rows := []AuditEntry{
		{Time: now, Actor: "admin@example.com", App: "web", Action: "restart", Result: "ok"},
		{Time: now.Add(-time.Second), Actor: "cli", App: "web", Action: "stop", Result: "error", Error: "boom"},
		{Time: now.Add(-2 * time.Second), Actor: "cli", App: "worker", Action: "start", Result: "ok"},
	}
	for _, row := range rows {
		if err := store.RecordAudit(row); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.SearchAudit(AuditFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Action != "restart" || all[0].Actor != "admin@example.com" {
		t.Fatalf("all = %+v", all)
	}
	for filter, want := range map[AuditFilter]int{
		{App: "web", Limit: 10}:      2,
		{Actor: "cli", Limit: 10}:    2,
		{Action: "start", Limit: 10}: 1,
	} {
		filter := filter
		found, err := store.SearchAudit(filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) != want {
			t.Errorf("filter %+v = %+v, want %d rows", filter, found, want)
		}
	}

	// An old row is dropped by the host prune; a fresh one survives.
	if err := store.RecordAudit(AuditEntry{Time: now.Add(-2 * time.Hour), Actor: "cli", App: "old", Action: "stop", Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(context.Background(), HostApp, time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.SearchAudit(AuditFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range remaining {
		if row.App == "old" {
			t.Fatalf("old audit row was not pruned: %+v", remaining)
		}
	}
}
