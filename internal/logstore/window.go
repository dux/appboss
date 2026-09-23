package logstore

import (
	"database/sql"
	"errors"
	"math"
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

// dbPath is where one app's database lives.
func (s *Store) dbPath(app string) string { return filepath.Join(s.dir, app, "dboss.sqlite") }

// exists reports whether app has a database on disk.
func (s *Store) exists(app string) (bool, error) {
	_, err := os.Stat(s.dbPath(app))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// reader opens the app database for reading without creating one. It returns a nil db, and no
// error, for an app that never logged; done releases a connection opened just for this read.
func (s *Store) reader(app string) (db *sql.DB, done func(), err error) {
	s.mu.Lock()
	w := s.apps[app]
	s.mu.Unlock()
	if w != nil {
		return w.db, func() {}, nil
	}
	if exists, err := s.exists(app); !exists {
		return nil, nil, err
	}
	db, err = sql.Open("sqlite", "file:"+filepath.ToSlash(s.dbPath(app))+"?mode=ro")
	if err != nil {
		return nil, nil, err
	}
	return db, func() { _ = db.Close() }, nil
}

// read runs fn against the app database without creating one; an app that never logged skips fn.
func (s *Store) read(app string, fn func(*sql.DB) error) error {
	db, done, err := s.reader(app)
	if err != nil || db == nil {
		return err
	}
	defer done()
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

// Rates counts requests in the last minute, hour and day.
func (s *Store) Rates(app string) (Rates, error) {
	db, done, err := s.reader(app)
	if err != nil || db == nil {
		return Rates{}, err
	}
	defer done()
	now := time.Now().UTC()
	var rates Rates
	for cutoff, target := range map[time.Duration]*int64{time.Minute: &rates.LastMinute, time.Hour: &rates.LastHour, 24 * time.Hour: &rates.LastDay} {
		if err := db.QueryRow(`SELECT count(*) FROM requests WHERE ts >= ?`, stamp(now.Add(-cutoff))).Scan(target); err != nil {
			return Rates{}, err
		}
	}
	return rates, nil
}

// percentile picks the nearest-rank value from an ascending slice: ceil(q*n)-1, clamped.
func percentile(sorted []int64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(q*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return float64(sorted[index])
}
