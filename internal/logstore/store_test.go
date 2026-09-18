package logstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordSearchAndPrune(t *testing.T) {
	dir := t.TempDir()
	store := New(dir, 5*time.Millisecond, nil, "")
	defer store.Close()

	if err := store.Record("demo", time.Hour, RequestEntry{Time: time.Now(), Method: "GET", Host: "demo.test", Path: "/hello", Status: 200, IP: "1.2.3.4", UserAgent: "curl", RequestID: "abc"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordLogs("demo", []LogEntry{{Time: time.Now(), Source: "process", Process: "web", Stream: "combined", Level: "error", Message: "boom request", RequestID: "abc", Raw: "boom request"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "demo", "dboss.sqlite")); err != nil {
		t.Fatalf("per-app database missing: %v", err)
	}

	requests := waitFor(t, func() ([]RequestEntry, error) { return store.SearchRequests("demo", RequestFilter{Query: "hello"}) })
	if len(requests) != 1 || requests[0].RequestID != "abc" || requests[0].Status != 200 {
		t.Fatalf("unexpected requests: %+v", requests)
	}
	logs := waitForLogs(t, func() ([]LogEntry, error) { return store.SearchLogs("demo", LogFilter{Query: "boom"}) })
	if len(logs) != 1 || logs[0].Process != "web" || logs[0].Level != "error" {
		t.Fatalf("unexpected logs: %+v", logs)
	}
	if filtered, err := store.SearchLogs("demo", LogFilter{Level: "error", Process: "web"}); err != nil || len(filtered) != 1 {
		t.Fatalf("process/level filter: %v %+v", err, filtered)
	}
	if filtered, err := store.SearchLogs("demo", LogFilter{Level: "warn"}); err != nil || len(filtered) != 0 {
		t.Fatalf("level filter should drop the row: %v %+v", err, filtered)
	}
	rates, err := store.Rates("demo")
	if err != nil || rates.LastMinute != 1 {
		t.Fatalf("unexpected rates: %v %+v", err, rates)
	}

	if err := store.Prune(context.Background(), "demo", time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if remaining, err := store.SearchLogs("demo", LogFilter{}); err != nil || len(remaining) != 0 {
		t.Fatalf("prune should delete old rows: %v %+v", err, remaining)
	}
}

func waitFor(t *testing.T, query func() ([]RequestEntry, error)) []RequestEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := query()
		if err == nil && len(rows) > 0 {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("no rows after flush: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForLogs(t *testing.T, query func() ([]LogEntry, error)) []LogEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := query()
		if err == nil && len(rows) > 0 {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log rows after flush: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
