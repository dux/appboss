package logstore

import (
	"database/sql"
	"time"
)

// trafficTop is how many rows every top list keeps.
const trafficTop = 15

// slowestMinHits keeps a path that was hit once out of the slowest list.
const slowestMinHits = 5

// Traffic is the request log of one app aggregated for the console's Traffic tab.
type Traffic struct {
	Since     time.Time       `json:"since"`
	Totals    Window          `json:"totals"`
	Bucket    string          `json:"bucket"`
	Series    []TrafficBucket `json:"series"`
	Paths     []TrafficPath   `json:"paths"`
	Slowest   []TrafficPath   `json:"slowest"`
	Statuses  []TrafficCount  `json:"statuses"`
	Countries []TrafficCount  `json:"countries"`
	IPs       []TrafficCount  `json:"ips"`
	Methods   []TrafficCount  `json:"methods"`
}

// TrafficBucket is one column of the requests-over-time chart, split by status class. Anything
// below 300 counts as S2 and anything from 500 up as S5.
type TrafficBucket struct {
	Time time.Time `json:"time"`
	S2   int64     `json:"s2"`
	S3   int64     `json:"s3"`
	S4   int64     `json:"s4"`
	S5   int64     `json:"s5"`
}

type TrafficPath struct {
	Path   string  `json:"path"`
	Count  int64   `json:"count"`
	Errors int64   `json:"errors"`
	AvgMS  float64 `json:"avg_ms"`
	MaxMS  int64   `json:"max_ms"`
}

type TrafficCount struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// trafficBucket picks the chart resolution from the range: the name, the step and how many
// leading characters of the RFC3339 ts identify one bucket.
func trafficBucket(span time.Duration) (string, time.Duration, int) {
	switch {
	case span <= 2*time.Hour:
		return "minute", time.Minute, 16
	case span <= 7*24*time.Hour:
		return "hour", time.Hour, 13
	default:
		return "day", 24 * time.Hour, 10
	}
}

// Traffic aggregates the requests newer than since. Every query is bounded by the ts index, and
// an app that never logged yields empty lists without creating a database.
func (s *Store) Traffic(app string, since time.Time) (Traffic, error) {
	now := time.Now().UTC()
	since = since.UTC()
	name, step, prefix := trafficBucket(now.Sub(since))
	traffic := Traffic{Since: since, Bucket: name, Paths: []TrafficPath{}, Slowest: []TrafficPath{}, Statuses: []TrafficCount{}, Countries: []TrafficCount{}, IPs: []TrafficCount{}, Methods: []TrafficCount{}}

	totals, err := s.Window(app, since)
	if err != nil {
		return Traffic{}, err
	}
	traffic.Totals = totals

	counts := map[string]TrafficBucket{}
	err = s.read(app, func(db *sql.DB) error {
		cutoff := stamp(since)
		rows, err := db.Query(`SELECT substr(ts, 1, ?), sum(status < 300), sum(status >= 300 AND status < 400), sum(status >= 400 AND status < 500), sum(status >= 500) FROM requests WHERE ts >= ? GROUP BY 1`, prefix, cutoff)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			var bucket TrafficBucket
			if err := rows.Scan(&key, &bucket.S2, &bucket.S3, &bucket.S4, &bucket.S5); err != nil {
				return err
			}
			counts[key] = bucket
		}
		if err := rows.Err(); err != nil {
			return err
		}

		const pathColumns = `path, count(*), sum(status >= 500), avg(duration_ms), max(duration_ms)`
		if traffic.Paths, err = trafficPaths(db, `SELECT `+pathColumns+` FROM requests WHERE ts >= ? GROUP BY path ORDER BY 2 DESC, path LIMIT ?`, cutoff, trafficTop); err != nil {
			return err
		}
		if traffic.Slowest, err = trafficPaths(db, `SELECT `+pathColumns+` FROM requests WHERE ts >= ? GROUP BY path HAVING count(*) >= ? ORDER BY 4 DESC, path LIMIT ?`, cutoff, slowestMinHits, trafficTop); err != nil {
			return err
		}
		for column, target := range map[string]*[]TrafficCount{"status": &traffic.Statuses, "country": &traffic.Countries, "ip": &traffic.IPs, "method": &traffic.Methods} {
			// column comes from the fixed map above, never from the request.
			if *target, err = trafficCounts(db, `SELECT CAST(`+column+` AS TEXT), count(*) FROM requests WHERE ts >= ? AND `+column+` <> '' GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT ?`, cutoff, trafficTop); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Traffic{}, err
	}

	// Every bucket of the range is present, so a gap in traffic reads as a gap in the chart.
	for at := since.Truncate(step); !at.After(now); at = at.Add(step) {
		bucket := counts[at.Format(time.RFC3339)[:prefix]]
		bucket.Time = at
		traffic.Series = append(traffic.Series, bucket)
	}
	return traffic, nil
}

func trafficPaths(db *sql.DB, query string, args ...any) ([]TrafficPath, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := []TrafficPath{}
	for rows.Next() {
		var path TrafficPath
		if err := rows.Scan(&path.Path, &path.Count, &path.Errors, &path.AvgMS, &path.MaxMS); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

func trafficCounts(db *sql.DB, query string, args ...any) ([]TrafficCount, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := []TrafficCount{}
	for rows.Next() {
		var count TrafficCount
		if err := rows.Scan(&count.Key, &count.Count); err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
}
