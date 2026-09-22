package logstore

import (
	"context"
	"testing"
	"time"
)

func TestAuditRecordSearchAndPrune(t *testing.T) {
	store := New(t.TempDir(), 5*time.Millisecond, nil, "", "", time.Hour, time.Hour)
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

	// Every row carries its own id, and an id addresses exactly that row.
	for _, row := range all {
		if row.ID == 0 {
			t.Fatalf("audit row has no id: %+v", row)
		}
	}
	one, err := store.SearchAudit(AuditFilter{ID: all[1].ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0] != all[1] {
		t.Fatalf("by id = %+v, want %+v", one, all[1])
	}
	missing, err := store.SearchAudit(AuditFilter{ID: 9999, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("unknown id = %+v", missing)
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

func TestLatencyQuantiles(t *testing.T) {
	store := New(t.TempDir(), 5*time.Millisecond, nil, "", "", time.Hour, time.Hour)
	defer store.Close()
	for _, duration := range []int64{10, 20, 30, 40, 100} {
		if err := store.Record("web", time.Hour, RequestEntry{Time: time.Now(), DurationMS: duration}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	latency, err := store.Latency("web", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if latency.Count != 5 || latency.P50 != 30 || latency.P95 != 100 || latency.P99 != 100 {
		t.Fatalf("latency = %+v", latency)
	}
}

func TestVacuumKeepsDatabaseUsable(t *testing.T) {
	store := New(t.TempDir(), 5*time.Millisecond, nil, "", "", time.Hour, time.Hour)
	defer store.Close()
	if err := store.RecordLogs("web", []LogEntry{{Time: time.Now(), Source: "stdout", Process: "web", Level: "info", Message: "hello"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := store.Vacuum(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	if err := store.Vacuum(context.Background(), "missing"); err != nil {
		t.Fatalf("missing database vacuum: %v", err)
	}
	logs, err := store.SearchLogs("web", LogFilter{Channel: "stdout", Limit: 5})
	if err != nil || len(logs) == 0 {
		t.Fatalf("logs after vacuum = %+v, %v", logs, err)
	}
}
