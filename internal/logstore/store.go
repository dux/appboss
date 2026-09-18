// Package logstore keeps every app's logs and request rows in one per-app SQLite database,
// batches writes in the background and prunes by retention. A ClickHouse or remote backend can
// implement the same surface later without touching the proxy, the supervisor or the console.
package logstore

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"deploy-boss/internal/super"
	_ "modernc.org/sqlite"
)

const queueSize = 4096

// RequestEntry is one proxied request.
type RequestEntry struct {
	Time       time.Time `json:"time"`
	Method     string    `json:"method"`
	Host       string    `json:"host"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	DurationMS int64     `json:"duration_ms"`
	BytesOut   int64     `json:"bytes_out"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
	RequestID  string    `json:"request_id"`
}

// LogEntry is one line of an app's process output.
type LogEntry struct {
	Time      time.Time `json:"time"`
	Source    string    `json:"source"`
	Process   string    `json:"process"`
	Stream    string    `json:"stream"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	RequestID string    `json:"request_id"`
	Raw       string    `json:"raw"`
}

type Rates struct {
	LastMinute int64
	LastHour   int64
	LastDay    int64
}

// LogFilter narrows SearchLogs. Query is full-text over message and raw.
type LogFilter struct {
	Process string
	Level   string
	Query   string
	Since   time.Time
	Limit   int
}

// RequestFilter narrows SearchRequests. Query matches host, path, ip or user agent.
type RequestFilter struct {
	Status int
	Query  string
	Since  time.Time
	Limit  int
}

// Snapshotter is the piece of the supervisor the prune loop needs: the apps and their retention.
type Snapshotter interface {
	Snapshots() []super.Snapshot
}

type entry struct {
	request *RequestEntry
	logs    []LogEntry
}

type appWriter struct {
	db      *sql.DB
	entries chan entry
	stop    chan struct{}
	done    chan struct{}
}

// Store is the per-app database manager and a daemon module.
type Store struct {
	dir         string
	flush       time.Duration
	snapshotter Snapshotter
	pruneAt     string
	mu          sync.Mutex
	apps        map[string]*appWriter
	ctx         context.Context
	cancel      context.CancelFunc
}

// New returns a store that writes under dir (one <app>/dboss.sqlite per app). snapshotter and
// pruneAt drive the daily retention prune; pass nil to disable it.
func New(dir string, flush time.Duration, snapshotter Snapshotter, pruneAt string) *Store {
	return &Store{dir: dir, flush: flush, snapshotter: snapshotter, pruneAt: pruneAt, apps: map[string]*appWriter{}}
}

func (s *Store) Name() string { return "logstore" }

// Start launches the retention prune loop. Databases open lazily on first write.
func (s *Store) Start(ctx context.Context) error {
	s.ctx, s.cancel = context.WithCancel(ctx)
	if s.snapshotter != nil {
		go s.pruneLoop()
	}
	return nil
}

func (s *Store) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Lock()
	writers := make([]*appWriter, 0, len(s.apps))
	for _, w := range s.apps {
		writers = append(writers, w)
	}
	s.mu.Unlock()
	var first error
	for _, w := range writers {
		close(w.stop)
		<-w.done
		if err := w.db.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Record queues one request row. retention <= 0 disables request logging for the app.
func (s *Store) Record(app string, retention time.Duration, e RequestEntry) error {
	if retention <= 0 {
		return nil
	}
	w, err := s.writer(app)
	if err != nil {
		return err
	}
	select {
	case w.entries <- entry{request: &e}:
		return nil
	default:
		return fmt.Errorf("request log queue full for %s", app)
	}
}

// RecordLogs queues a batch of process-log rows.
func (s *Store) RecordLogs(app string, entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	w, err := s.writer(app)
	if err != nil {
		return err
	}
	select {
	case w.entries <- entry{logs: entries}:
		return nil
	default:
		return fmt.Errorf("log queue full for %s", app)
	}
}

func (s *Store) writer(app string) (*appWriter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.apps[app]; w != nil {
		return w, nil
	}
	dir := filepath.Join(s.dir, app)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "dboss.sqlite"))
	if err != nil {
		return nil, err
	}
	for _, statement := range schema {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	w := &appWriter{db: db, entries: make(chan entry, queueSize), stop: make(chan struct{}), done: make(chan struct{})}
	s.apps[app] = w
	go w.loop(s.flush)
	return w, nil
}

var schema = []string{
	`PRAGMA journal_mode=WAL`,
	`PRAGMA busy_timeout=5000`,
	`CREATE TABLE IF NOT EXISTS requests (ts TEXT NOT NULL, method TEXT NOT NULL, host TEXT NOT NULL, path TEXT NOT NULL, status INTEGER NOT NULL, duration_ms INTEGER NOT NULL, bytes_out INTEGER NOT NULL, ip TEXT NOT NULL, ua TEXT NOT NULL, request_id TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX IF NOT EXISTS requests_ts ON requests(ts)`,
	`CREATE TABLE IF NOT EXISTS logs (ts TEXT NOT NULL, source TEXT NOT NULL, process TEXT NOT NULL, stream TEXT NOT NULL, level TEXT NOT NULL, message TEXT NOT NULL, request_id TEXT NOT NULL, raw TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS logs_ts ON logs(ts)`,
	`CREATE INDEX IF NOT EXISTS logs_process ON logs(process)`,
	`CREATE INDEX IF NOT EXISTS logs_level ON logs(level)`,
	`CREATE VIRTUAL TABLE IF NOT EXISTS logs_fts USING fts5(message, raw, content='logs', content_rowid='rowid')`,
	`CREATE TRIGGER IF NOT EXISTS logs_ai AFTER INSERT ON logs BEGIN INSERT INTO logs_fts(rowid, message, raw) VALUES (new.rowid, new.message, new.raw); END`,
	`CREATE TRIGGER IF NOT EXISTS logs_ad AFTER DELETE ON logs BEGIN INSERT INTO logs_fts(logs_fts, rowid, message, raw) VALUES ('delete', old.rowid, old.message, old.raw); END`,
}

func (w *appWriter) loop(flush time.Duration) {
	defer close(w.done)
	ticker := time.NewTicker(flush)
	defer ticker.Stop()
	var requests []RequestEntry
	var logs []LogEntry
	flushNow := func() {
		if len(requests) == 0 && len(logs) == 0 {
			return
		}
		if err := w.insert(requests, logs); err != nil {
			log.Printf("logstore insert: %v", err)
		}
		requests, logs = requests[:0], logs[:0]
	}
	drain := func(op entry) {
		if op.request != nil {
			requests = append(requests, *op.request)
		}
		if len(op.logs) > 0 {
			logs = append(logs, op.logs...)
		}
	}
	for {
		select {
		case op := <-w.entries:
			drain(op)
			if len(requests) >= 256 {
				flushNow()
			}
		case <-ticker.C:
			flushNow()
		case <-w.stop:
			for {
				select {
				case op := <-w.entries:
					drain(op)
				default:
					flushNow()
					return
				}
			}
		}
	}
}

func (w *appWriter) insert(requests []RequestEntry, logs []LogEntry) error {
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}
	if len(requests) > 0 {
		statement, err := tx.Prepare(`INSERT INTO requests (ts, method, host, path, status, duration_ms, bytes_out, ip, ua, request_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		for _, e := range requests {
			if _, err := statement.Exec(stamp(e.Time), e.Method, e.Host, e.Path, e.Status, e.DurationMS, e.BytesOut, e.IP, e.UserAgent, e.RequestID); err != nil {
				_ = statement.Close()
				_ = tx.Rollback()
				return err
			}
		}
		_ = statement.Close()
	}
	if len(logs) > 0 {
		statement, err := tx.Prepare(`INSERT INTO logs (ts, source, process, stream, level, message, request_id, raw) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		for _, e := range logs {
			if _, err := statement.Exec(stamp(e.Time), e.Source, e.Process, e.Stream, e.Level, e.Message, e.RequestID, e.Raw); err != nil {
				_ = statement.Close()
				_ = tx.Rollback()
				return err
			}
		}
		_ = statement.Close()
	}
	return tx.Commit()
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// SearchLogs returns the newest matching rows, newest first.
func (s *Store) SearchLogs(app string, filter LogFilter) ([]LogEntry, error) {
	w, err := s.writer(app)
	if err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	where := []string{}
	args := []any{}
	if filter.Process != "" {
		where = append(where, "logs.process = ?")
		args = append(args, filter.Process)
	}
	if filter.Level != "" {
		where = append(where, "logs.level = ?")
		args = append(args, filter.Level)
	}
	if !filter.Since.IsZero() {
		where = append(where, "logs.ts >= ?")
		args = append(args, stamp(filter.Since))
	}
	from := "logs"
	if filter.Query != "" {
		from = "logs JOIN logs_fts ON logs_fts.rowid = logs.rowid"
		where = append(where, "logs_fts MATCH ?")
		args = append(args, ftsQuery(filter.Query))
	}
	query := `SELECT logs.ts, logs.source, logs.process, logs.stream, logs.level, logs.message, logs.request_id, logs.raw FROM ` + from
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY logs.ts DESC LIMIT ?"
	args = append(args, limit)
	rows, err := w.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []LogEntry
	for rows.Next() {
		var e LogEntry
		var ts string
		if err := rows.Scan(&ts, &e.Source, &e.Process, &e.Stream, &e.Level, &e.Message, &e.RequestID, &e.Raw); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, ts)
		result = append(result, e)
	}
	return result, rows.Err()
}

// SearchRequests returns the newest matching rows, newest first.
func (s *Store) SearchRequests(app string, filter RequestFilter) ([]RequestEntry, error) {
	w, err := s.writer(app)
	if err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	where := []string{}
	args := []any{}
	if filter.Status != 0 {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if !filter.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, stamp(filter.Since))
	}
	if filter.Query != "" {
		where = append(where, "(host LIKE ? OR path LIKE ? OR ip LIKE ? OR ua LIKE ?)")
		like := "%" + filter.Query + "%"
		args = append(args, like, like, like, like)
	}
	query := `SELECT ts, method, host, path, status, duration_ms, bytes_out, ip, ua, request_id FROM requests`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY ts DESC LIMIT ?"
	args = append(args, limit)
	rows, err := w.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RequestEntry
	for rows.Next() {
		var e RequestEntry
		var ts string
		if err := rows.Scan(&ts, &e.Method, &e.Host, &e.Path, &e.Status, &e.DurationMS, &e.BytesOut, &e.IP, &e.UserAgent, &e.RequestID); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, ts)
		result = append(result, e)
	}
	return result, rows.Err()
}

// ftsQuery wraps a user phrase so each token is a prefix match and keeps FTS syntax out of the
// query. Without the quoting a stray " would turn into an FTS error.
func ftsQuery(query string) string {
	tokens := strings.Fields(query)
	for i, token := range tokens {
		tokens[i] = `"` + strings.ReplaceAll(token, `"`, `""`) + `"*`
	}
	return strings.Join(tokens, " ")
}

// Rates counts requests in the last minute, hour and day.
func (s *Store) Rates(app string) (Rates, error) {
	w, err := s.writer(app)
	if err != nil {
		return Rates{}, err
	}
	now := time.Now().UTC()
	var rates Rates
	for cutoff, target := range map[time.Duration]*int64{time.Minute: &rates.LastMinute, time.Hour: &rates.LastHour, 24 * time.Hour: &rates.LastDay} {
		if err := w.db.QueryRow(`SELECT count(*) FROM requests WHERE ts >= ?`, stamp(now.Add(-cutoff))).Scan(target); err != nil {
			return Rates{}, err
		}
	}
	return rates, nil
}

// Prune deletes rows older than retention from one app's database.
func (s *Store) Prune(ctx context.Context, app string, retention time.Duration) error {
	if retention <= 0 {
		return nil
	}
	s.mu.Lock()
	w := s.apps[app]
	s.mu.Unlock()
	if w == nil {
		return nil
	}
	cutoff := stamp(time.Now().Add(-retention))
	for _, table := range []string{"requests", "logs"} {
		if _, err := w.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE ts < ?`, cutoff); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) pruneLoop() {
	for {
		now := time.Now()
		target, _ := time.ParseInLocation("15:04", s.pruneAt, now.Location())
		next := time.Date(now.Year(), now.Month(), now.Day(), target.Hour(), target.Minute(), 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			for _, snapshot := range s.snapshotter.Snapshots() {
				if err := s.Prune(s.ctx, snapshot.Name, snapshot.LogRetention); err != nil {
					log.Printf("log prune %s: %v", snapshot.Name, err)
				}
			}
		}
	}
}
