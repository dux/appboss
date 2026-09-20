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

func TestKeepSetRetainsTheDayWindow(t *testing.T) {
	// One dump a day for 40 days, ending now.
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	entries := entriesAt(now, 24*time.Hour, 40)
	keep := keepSet(entries, 30, now)

	for _, id := range []string{"b000", "b029"} {
		if !keep[id] {
			t.Errorf("%s is inside the 30-day window and should be kept", id)
		}
	}
	if keep["b031"] {
		t.Errorf("b031 is older than 30 days and should be pruned")
	}
}

func TestKeepSetZeroDaysKeepsEverything(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	entries := entriesAt(now, 24*time.Hour, 40)
	if keep := keepSet(entries, 0, now); len(keep) != len(entries) {
		t.Fatalf("days 0 must keep every dump, kept %d of %d", len(keep), len(entries))
	}
}

func TestKeepSetIgnoresFailedAndBadTimes(t *testing.T) {
	now := time.Now().UTC()
	entries := []Backup{
		{ID: "ok", Status: "ok", Time: now.Format(time.RFC3339)},
		{ID: "failed", Status: "failed", Time: now.Format(time.RFC3339)},
		{ID: "bad", Status: "ok", Time: "not a time"},
	}
	keep := keepSet(entries, 30, now)
	if !keep["ok"] {
		t.Fatal("the successful entry should be kept")
	}
	if keep["failed"] || keep["bad"] {
		t.Fatal("failed or unparseable entries must not be kept")
	}
}
