package schedule

import (
	"context"
	"time"
)

// NextDaily is the next occurrence of the local time of day at ("15:04"), strictly after now. An
// empty or invalid at returns the zero time.
func NextDaily(at string, now time.Time) time.Time {
	if at == "" {
		return time.Time{}
	}
	moment, err := time.Parse("15:04", at)
	if err != nil {
		return time.Time{}
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), moment.Hour(), moment.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// Daily calls run at every local time of day at until ctx ends. An empty or invalid at returns at
// once without running.
func Daily(ctx context.Context, at string, run func()) {
	for {
		next := NextDaily(at, time.Now())
		if next.IsZero() {
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			run()
		}
	}
}
