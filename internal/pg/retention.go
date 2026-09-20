package pg

import "time"

// keepSet returns the ids to keep from entries: every successful dump newer than now minus days.
// days <= 0 keeps every successful dump. Entries must be newest first.
func keepSet(entries []Backup, days int, now time.Time) map[string]bool {
	keep := map[string]bool{}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	for _, entry := range entries {
		if entry.Status != "ok" {
			continue
		}
		moment, err := time.Parse(time.RFC3339, entry.Time)
		if err != nil {
			continue
		}
		if days > 0 && moment.Before(cutoff) {
			continue
		}
		keep[entry.ID] = true
	}
	return keep
}
