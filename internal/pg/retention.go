package pg

import "time"

// rotation windows. A scheduled dump older than its database's window is pruned; a manual dump is
// never pruned.
const (
	weekWindow  = 7 * 24 * time.Hour
	monthWindow = 30 * 24 * time.Hour
)

// rotationWindow maps a configured rotation to its retention window. An unknown or empty rotation
// is treated as week, so a database left in the catalog after its config entry was removed is still
// bounded.
func rotationWindow(rotation string) time.Duration {
	if rotation == "month" {
		return monthWindow
	}
	return weekWindow
}

// keepSet returns the ids to keep from entries. A manual dump is always kept, a scheduled dump is
// kept while it is younger than window, and a non-positive window keeps everything. Entries must be
// newest first.
func keepSet(entries []Backup, window time.Duration, now time.Time) map[string]bool {
	keep := map[string]bool{}
	cutoff := now.Add(-window)
	for _, entry := range entries {
		if entry.Manual || window <= 0 {
			keep[entry.ID] = true
			continue
		}
		if entry.Status != "ok" {
			continue
		}
		moment, err := time.Parse(time.RFC3339, entry.Time)
		if err != nil {
			continue
		}
		if moment.Before(cutoff) {
			continue
		}
		keep[entry.ID] = true
	}
	return keep
}
