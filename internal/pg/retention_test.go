package pg

import (
	"fmt"
	"testing"
	"time"
)

// entriesAt builds one backup per step, newest first, like the catalog returns them.
func entriesAt(start time.Time, step time.Duration, count int) []Backup {
	var entries []Backup
	for i := 0; i < count; i++ {
		moment := start.Add(-time.Duration(i) * step)
		entries = append(entries, Backup{ID: fmt.Sprintf("b%03d", i), Status: "ok", Time: moment.UTC().Format(time.RFC3339)})
	}
	return entries
}

func TestKeepSetKeepsNewestPerBucket(t *testing.T) {
	// 48 hourly backups ending at noon, so 2 calendar days and many hours.
	start := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	entries := entriesAt(start, time.Hour, 48)
	keep := keepSet(entries, Policy{Hourly: 3, Daily: 2, Weekly: 1, Monthly: 1})

	for _, id := range []string{"b000", "b001", "b002"} {
		if !keep[id] {
			t.Errorf("newest hourly %s should be kept", id)
		}
	}
	if keep["b003"] {
		t.Errorf("fourth hourly should be pruned")
	}
	if !keep["b013"] {
		t.Errorf("the newest backup of the previous day should be kept by the daily bucket")
	}
	if keep["b036"] {
		t.Errorf("only the two newest days fit the daily bucket, so b036 must be pruned")
	}
}

func TestKeepSetIgnoresFailedAndBadTimes(t *testing.T) {
	now := time.Now().UTC()
	entries := []Backup{
		{ID: "ok", Status: "ok", Time: now.Format(time.RFC3339)},
		{ID: "failed", Status: "failed", Time: now.Format(time.RFC3339)},
		{ID: "bad", Status: "ok", Time: "not a time"},
	}
	keep := keepSet(entries, Policy{Monthly: 1})
	if !keep["ok"] {
		t.Fatal("the successful entry should be kept")
	}
	if keep["failed"] || keep["bad"] {
		t.Fatal("failed or unparseable entries must not be kept")
	}
}

func TestKeepSetZeroPolicyKeepsNothing(t *testing.T) {
	entries := entriesAt(time.Now().UTC(), time.Hour, 5)
	if keep := keepSet(entries, Policy{}); len(keep) != 0 {
		t.Fatalf("a zero policy must keep nothing, got %v", keep)
	}
}
