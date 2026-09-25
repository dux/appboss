package logstore

import (
	"database/sql"
	"strings"
)

// BlockedLimit caps the counter list the console shows; the biggest paths are kept.
const BlockedLimit = 200

// BlockedStat is one deny path and how many requests it turned away, counted across apps.
type BlockedStat struct {
	Path  string `json:"path"`
	Count int64  `json:"count"`
}

// Blocked lists the deny counters, biggest first, capped at BlockedLimit. The table lives in the
// reserved host database and is missing on a host that never blocked a request or predates the
// feature, which reads as empty.
func (s *Store) Blocked() ([]BlockedStat, error) {
	stats := []BlockedStat{}
	err := s.read(HostApp, func(db *sql.DB) error {
		rows, err := db.Query(`SELECT path, count FROM blocked ORDER BY count DESC, path LIMIT ?`, BlockedLimit)
		if err != nil {
			if strings.Contains(err.Error(), "no such table") {
				return nil
			}
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var stat BlockedStat
			if err := rows.Scan(&stat.Path, &stat.Count); err != nil {
				return err
			}
			stats = append(stats, stat)
		}
		return rows.Err()
	})
	return stats, err
}
