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
	store := New(dir, 5*time.Millisecond, nil, "", time.Hour)
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

	if err := store.Prune(context.Background(), "demo", time.Nanosecond, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if remaining, err := store.SearchLogs("demo", LogFilter{}); err != nil || len(remaining) != 0 {
		t.Fatalf("prune should delete old rows: %v %+v", err, remaining)
	}
}

func TestChannelsFilterAndPruneBySource(t *testing.T) {
	dir := t.TempDir()
	store := New(dir, 5*time.Millisecond, nil, "", time.Hour)
	defer store.Close()

	now := time.Now()
	if err := store.RecordLogs("demo", []LogEntry{
		{Time: now, Source: "stdout", Process: "web", Level: "info", Message: "boot", Raw: "boot"},
		{Time: now, Source: "file", Process: "production.log", Level: "info", Message: "served", Raw: "served"},
	}); err != nil {
		t.Fatal(err)
	}

	waitForLogs(t, func() ([]LogEntry, error) {
		return store.SearchLogs("demo", LogFilter{Channel: "file:production.log"})
	})

	channels, err := store.Channels("demo")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, channel := range channels {
		ids[channel.ID] = true
	}
	for _, want := range []string{"request", "stdout", "file:production.log"} {
		if !ids[want] {
			t.Fatalf("missing channel %q in %+v", want, channels)
		}
	}
	hostChannels, err := store.Channels(HostApp)
	if err != nil || len(hostChannels) != 1 || hostChannels[0].ID != "dboss" {
		t.Fatalf("host channels: %v %+v", err, hostChannels)
	}

	fileRows, err := store.SearchLogs("demo", LogFilter{Channel: "file:production.log"})
	if err != nil || len(fileRows) != 1 || fileRows[0].Message != "served" {
		t.Fatalf("file channel: %v %+v", err, fileRows)
	}

	if err := store.Prune(context.Background(), "demo", time.Hour, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	stdout, err := store.SearchLogs("demo", LogFilter{Channel: "stdout"})
	if err != nil || len(stdout) != 0 {
		t.Fatalf("stdout should be pruned: %v %+v", err, stdout)
	}
	files, err := store.SearchLogs("demo", LogFilter{Channel: "file:production.log"})
	if err != nil || len(files) != 1 {
		t.Fatalf("file should survive: %v %+v", err, files)
	}
}

func TestTailOffsetsRoundTrip(t *testing.T) {
	store := New(t.TempDir(), 5*time.Millisecond, nil, "", time.Hour)
	defer store.Close()

	path := "/app/log/production.log"
	if err := store.SaveTailOffset("demo", path, 42, 128); err != nil {
		t.Fatal(err)
	}
	offsets, err := store.TailOffsets("demo")
	if err != nil || offsets[path].Offset != 128 || offsets[path].Inode != 42 {
		t.Fatalf("unexpected offsets: %v %+v", err, offsets)
	}
	if err := store.SaveTailOffset("demo", path, 43, 200); err != nil {
		t.Fatal(err)
	}
	offsets, _ = store.TailOffsets("demo")
	if offsets[path].Offset != 200 || offsets[path].Inode != 43 {
		t.Fatalf("offset should update: %+v", offsets)
	}
	if err := store.RemoveTailOffsets("demo", []string{path}); err != nil {
		t.Fatal(err)
	}
	offsets, _ = store.TailOffsets("demo")
	if len(offsets) != 0 {
		t.Fatalf("offset should be removed: %+v", offsets)
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
