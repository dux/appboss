package ops

import (
	"time"

	"dboss/internal/logstore"
	"dboss/internal/metrics"
)

// Metrics gathers one /metrics scrape from the sources the console reads, so the two can never
// disagree. A missing source leaves its part of the input empty.
func (s *Service) Metrics() metrics.Input {
	now := time.Now()
	input := metrics.Input{Apps: s.Apps(), Now: now, Latency: map[string]logstore.Window{}, Pubsub: s.pubsubStats(), Postgres: s.pgMetrics()}
	if s.notifier != nil {
		input.Notify = s.notifier.Stats()
	}
	if s.store != nil {
		for _, app := range input.Apps {
			if window, err := s.store.Window(app.Name, now.Add(-time.Hour)); err == nil {
				input.Latency[app.Name] = window
			}
		}
	}
	return input
}

// pgMetrics reduces the cached PostgreSQL inspection and backup catalog; it never touches the
// server, so a scrape stays cheap.
func (s *Service) pgMetrics() metrics.PGStats {
	var stats metrics.PGStats
	snapshot, err := s.PGSnapshot(false)
	if err != nil {
		return stats
	}
	stats.Up = snapshot.Available
	for _, database := range snapshot.Databases {
		stats.Databases = append(stats.Databases, metrics.PGDatabase{Name: database.Name, SizeBytes: database.SizeBytes})
	}
	for _, entry := range s.Backups() {
		moment, _ := time.Parse(time.RFC3339, entry.Time)
		stats.Backups = append(stats.Backups, metrics.PGBackup{Database: entry.Database, Time: moment, Status: entry.Status})
	}
	return stats
}
