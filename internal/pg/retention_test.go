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

func TestKeepSetRetainsTheWindow(t *testing.T) {
	// One dump a day for 40 days, ending now.
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	entries := entriesAt(now, 24*time.Hour, 40)
	keep := keepSet(entries, monthWindow, now)

	for _, id := range []string{"b000", "b029"} {
		if !keep[id] {
			t.Errorf("%s is inside the 30-day window and should be kept", id)
		}
	}
	if keep["b031"] {
		t.Errorf("b031 is older than 30 days and should be pruned")
	}
}

func TestKeepSetManualEntriesSurvive(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	entries := []Backup{
		{ID: "recent", Status: "ok", Time: now.Add(-time.Hour).Format(time.RFC3339)},
		{ID: "old", Status: "ok", Time: now.Add(-90 * 24 * time.Hour).Format(time.RFC3339)},
		{ID: "manual", Status: "ok", Manual: true, Time: now.Add(-900 * 24 * time.Hour).Format(time.RFC3339)},
	}
	keep := keepSet(entries, weekWindow, now)
	if !keep["recent"] || !keep["manual"] {
		t.Fatalf("recent and manual entries must be kept, got %v", keep)
	}
	if keep["old"] {
		t.Fatalf("the old scheduled entry must be pruned, got %v", keep)
	}
}

func TestKeepSetZeroWindowKeepsEverything(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	entries := entriesAt(now, 24*time.Hour, 40)
	if keep := keepSet(entries, 0, now); len(keep) != len(entries) {
		t.Fatalf("a zero window must keep every dump, kept %d of %d", len(keep), len(entries))
	}
}

func TestKeepSetIgnoresFailedAndBadTimes(t *testing.T) {
	now := time.Now().UTC()
	entries := []Backup{
		{ID: "ok", Status: "ok", Time: now.Format(time.RFC3339)},
		{ID: "failed", Status: "failed", Time: now.Format(time.RFC3339)},
		{ID: "bad", Status: "ok", Time: "not a time"},
	}
	keep := keepSet(entries, monthWindow, now)
	if !keep["ok"] {
		t.Fatal("the successful entry should be kept")
	}
	if keep["failed"] || keep["bad"] {
		t.Fatal("failed or unparseable entries must not be kept")
	}
}

func TestRotationWindow(t *testing.T) {
	if got := rotationWindow("month"); got != monthWindow {
		t.Fatalf("month window = %s", got)
	}
	for _, rotation := range []string{"week", "", "unknown"} {
		if got := rotationWindow(rotation); got != weekWindow {
			t.Fatalf("rotation %q window = %s, want week", rotation, got)
		}
	}
}
