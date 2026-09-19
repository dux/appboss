package pg

import (
	"fmt"
	"time"
)

// Policy is the grandfather-father-son retention: the newest KeepFor each bucket survives.
type Policy struct {
	Hourly  int
	Daily   int
	Weekly  int
	Monthly int
}

// keepSet returns the ids to keep from entries. Entries must already be newest first. Each bucket
// keeps the newest entry for its first N distinct periods; the union across buckets is kept.
func keepSet(entries []Backup, policy Policy) map[string]bool {
	keep := map[string]bool{}
	for _, bucket := range []struct {
		limit  int
		period func(time.Time) string
	}{
		{policy.Hourly, func(t time.Time) string { return t.Format("2006-01-02T15") }},
		{policy.Daily, func(t time.Time) string { return t.Format("2006-01-02") }},
		{policy.Weekly, func(t time.Time) string { year, week := t.ISOWeek(); return fmt.Sprintf("%d-%02d", year, week) }},
		{policy.Monthly, func(t time.Time) string { return t.Format("2006-01") }},
	} {
		if bucket.limit <= 0 {
			continue
		}
		seen := map[string]bool{}
		kept := 0
		for _, entry := range entries {
			if entry.Status != "ok" {
				continue
			}
			moment, err := time.Parse(time.RFC3339, entry.Time)
			if err != nil {
				continue
			}
			period := bucket.period(moment)
			if seen[period] {
				continue
			}
			seen[period] = true
			keep[entry.ID] = true
			kept++
			if kept >= bucket.limit {
				break
			}
		}
	}
	return keep
}
