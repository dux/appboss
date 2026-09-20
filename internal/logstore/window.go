package logstore

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Window summarizes the requests newer than a cutoff. Count, Errors (5xx answers) and BytesOut
// are exact; the quantiles are milliseconds over the newest latencySampleLimit rows and 0 when
// the window is empty.
type Window struct {
	Count    int64   `json:"count"`
	Errors   int64   `json:"errors"`
	BytesOut int64   `json:"bytes_out"`
	P50      float64 `json:"p50"`
	P95      float64 `json:"p95"`
	P99      float64 `json:"p99"`
}

// ErrorRate is the share of 5xx answers in percent, 0 for an empty window.
func (w Window) ErrorRate() float64 {
	if w.Count == 0 {
		return 0
	}
	return float64(w.Errors) * 100 / float64(w.Count)
}

// read runs fn against the app database without creating one; an app that never logged skips fn.
func (s *Store) read(app string, fn func(*sql.DB) error) error {
	s.mu.Lock()
	w := s.apps[app]
	s.mu.Unlock()
	if w != nil {
		return fn(w.db)
	}
	path := filepath.Join(s.dir, app, "dboss.sqlite")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	return fn(db)
}

// Window reads the request summary for rows newer than since.
func (s *Store) Window(app string, since time.Time) (Window, error) {
	var window Window
	err := s.read(app, func(db *sql.DB) error {
		cutoff := stamp(since)
		if err := db.QueryRow(`SELECT count(*), coalesce(sum(status >= 500), 0), coalesce(sum(bytes_out), 0) FROM requests WHERE ts >= ?`, cutoff).Scan(&window.Count, &window.Errors, &window.BytesOut); err != nil {
			return err
		}
		rows, err := db.Query(`SELECT duration_ms FROM requests WHERE ts >= ? ORDER BY ts DESC LIMIT ?`, cutoff, latencySampleLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		durations := make([]int64, 0, 1024)
		for rows.Next() {
			var duration int64
			if err := rows.Scan(&duration); err != nil {
				return err
			}
			durations = append(durations, duration)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		slices.Sort(durations)
		window.P50, window.P95, window.P99 = percentile(durations, 0.5), percentile(durations, 0.95), percentile(durations, 0.99)
		return nil
	})
	return window, err
}
