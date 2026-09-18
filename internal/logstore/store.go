// Package logstore keeps every app's logs and request rows in one per-app SQLite database,
// batches writes in the background and prunes by retention. A ClickHouse or remote backend can
// implement the same surface later without touching the proxy, the supervisor or the console.
package logstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"app-boss/internal/logx"
	"app-boss/internal/super"
	_ "modernc.org/sqlite"
)

const queueSize = 4096

// Cap on rows kept for retry when the database is temporarily unwritable. Beyond it the oldest
// rows are dropped so a permanently broken database cannot grow the heap without bound.
const (
	maxBufferedRequests = 8192
	maxBufferedLogs     = 32768
)

// RequestEntry is one proxied request. Process is the service that answered it.
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
	Process    string    `json:"process"`
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

// Latency summarizes request durations over a window, in milliseconds. Count is the sample size
// (capped) and the quantiles are 0 when it is 0.
type Latency struct {
	Count int
	P50   float64
	P95   float64
	P99   float64
}

// latencySampleLimit bounds how many rows a latency query reads, newest first.
const latencySampleLimit = 50000

// HostApp is the reserved app name that backs appboss's own daemon log. It never collides with a
// discovered app because app process names must match [a-z][a-z0-9_-]*.
const HostApp = "_appboss"

// Channel is one selectable log type in the console: the request table, process stdout, the
// appboss daemon log, or one app log file.
type Channel struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// AppTree is one app in the log viewer's left nav: the sqlite files on disk and the channels
// inside them.
type AppTree struct {
	Name     string    `json:"name"`
	Bytes    int64     `json:"bytes"`
	Channels []Channel `json:"channels"`
}

// TailOffset is how far the file tailer has read into one app log file.
type TailOffset struct {
	Path   string
	Inode  uint64
	Offset int64
}

// LogFilter narrows SearchLogs. Channel selects one log type: "stdout", "appboss" or "file:<path>".
// Query is full-text over message and raw.
type LogFilter struct {
	Channel string
	Process string
	Level   string
	Query   string
	Since   time.Time
	Before  time.Time
	Limit   int
}

// RequestFilter narrows SearchRequests. Query matches host, path, ip or user agent. Status is an
// exact code; StatusClass (200, 300, 400 or 500) matches a whole class. Process selects the
// service that answered.
type RequestFilter struct {
	Method      string
	Process     string
	Status      int
	StatusClass int
	Query       string
	Since       time.Time
	Before      time.Time
	Limit       int
}

// AuditEntry is one operator action: who did what to which app, and how it turned out. Audit rows
// live in the reserved host database.
type AuditEntry struct {
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	App    string    `json:"app"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
	Result string    `json:"result"`
	Error  string    `json:"error,omitempty"`
}

// AuditFilter narrows SearchAudit.
type AuditFilter struct {
	App    string
	Actor  string
	Action string
	Since  time.Time
	Before time.Time
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
	dir            string
	flush          time.Duration
	snapshotter    Snapshotter
	pruneAt        string
	vacuumAt       string
	hostRetention  time.Duration
	auditRetention time.Duration
	mu             sync.Mutex
	apps           map[string]*appWriter
	ctx            context.Context
	cancel         context.CancelFunc
}

// New returns a store that writes under dir (one <app>/appboss.sqlite per app). snapshotter and
// pruneAt drive the daily retention prune; pass nil to disable it. vacuumAt schedules the daily
// VACUUM (empty disables it). hostRetention bounds the reserved HostApp database that holds
// appboss's own daemon log; auditRetention bounds the audit table (0 keeps audit rows forever).
func New(dir string, flush time.Duration, snapshotter Snapshotter, pruneAt, vacuumAt string, hostRetention, auditRetention time.Duration) *Store {
	return &Store{dir: dir, flush: flush, snapshotter: snapshotter, pruneAt: pruneAt, vacuumAt: vacuumAt, hostRetention: hostRetention, auditRetention: auditRetention, apps: map[string]*appWriter{}}
}

func (s *Store) Name() string { return "logstore" }

// Start launches the retention prune loop and the daily vacuum. Databases open lazily on first
// write.
func (s *Store) Start(ctx context.Context) error {
	s.ctx, s.cancel = context.WithCancel(ctx)
	if s.snapshotter != nil {
		go s.pruneLoop()
		if s.vacuumAt != "" {
			go s.vacuumLoop()
		}
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

// AppendLogs inserts a batch of process-log rows and waits for the commit. The file tailer and
// sealed-segment ingester use it so they only delete a segment or advance an offset once its rows
// are durably stored; RecordLogs is the fire-and-forget path for rows whose source can be retried.
func (s *Store) AppendLogs(app string, entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	w, err := s.writer(app)
	if err != nil {
		return err
	}
	return w.insert(nil, entries)
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
	db, err := sql.Open("sqlite", filepath.Join(dir, "appboss.sqlite"))
	if err != nil {
		return nil, err
	}
	for _, statement := range schema {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	// Databases written before requests carried a process are missing the column; the log cache is
	// disposable but a failed insert would silently drop rows, so add it in place once.
	_, _ = db.Exec(`ALTER TABLE requests ADD COLUMN process TEXT NOT NULL DEFAULT ''`)
	w := &appWriter{db: db, entries: make(chan entry, queueSize), stop: make(chan struct{}), done: make(chan struct{})}
	s.apps[app] = w
	go w.loop(s.flush)
	return w, nil
}

// writerForPrune returns the writer for an app, opening an existing database without creating a
// new one. The host database is always opened because its audit table outlives any log rows.
func (s *Store) writerForPrune(app string) (*appWriter, error) {
	s.mu.Lock()
	w := s.apps[app]
	s.mu.Unlock()
	if w != nil {
		return w, nil
	}
	if app != HostApp {
		if _, err := os.Stat(filepath.Join(s.dir, app, "appboss.sqlite")); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
	}
	return s.writer(app)
}

// diskApps lists every app with a database on disk, including apps that were removed from config.
func (s *Store) diskApps() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(s.dir, entry.Name(), "appboss.sqlite")); statErr == nil {
			names = append(names, entry.Name())
		}
	}
	return names
}

var schema = []string{
	`PRAGMA journal_mode=WAL`,
	`PRAGMA busy_timeout=5000`,
	`CREATE TABLE IF NOT EXISTS requests (ts TEXT NOT NULL, method TEXT NOT NULL, host TEXT NOT NULL, path TEXT NOT NULL, status INTEGER NOT NULL, duration_ms INTEGER NOT NULL, bytes_out INTEGER NOT NULL, ip TEXT NOT NULL, ua TEXT NOT NULL, request_id TEXT NOT NULL DEFAULT '', process TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX IF NOT EXISTS requests_ts ON requests(ts)`,
	`CREATE INDEX IF NOT EXISTS requests_process ON requests(process)`,
	`CREATE TABLE IF NOT EXISTS logs (ts TEXT NOT NULL, source TEXT NOT NULL, process TEXT NOT NULL, stream TEXT NOT NULL, level TEXT NOT NULL, message TEXT NOT NULL, request_id TEXT NOT NULL, raw TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS logs_ts ON logs(ts)`,
	`CREATE INDEX IF NOT EXISTS logs_process ON logs(process)`,
	`CREATE INDEX IF NOT EXISTS logs_level ON logs(level)`,
	`CREATE INDEX IF NOT EXISTS logs_source ON logs(source)`,
	`CREATE TABLE IF NOT EXISTS tail_offsets (path TEXT PRIMARY KEY, inode INTEGER NOT NULL, offset INTEGER NOT NULL, updated_ts TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS audit (ts TEXT NOT NULL, actor TEXT NOT NULL, app TEXT NOT NULL, action TEXT NOT NULL, detail TEXT NOT NULL, result TEXT NOT NULL, error TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS audit_ts ON audit(ts)`,
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
			// Keep the batch for the next tick so a transient failure does not lose rows; cap
			// it so a permanently broken database cannot grow the buffer without bound.
			logx.Errorf("logstore insert: %v", err)
			if len(requests) > maxBufferedRequests {
				drop := len(requests) - maxBufferedRequests
				logx.Warnf("logstore: dropping %d request rows after repeated insert failures", drop)
				requests = requests[:copy(requests, requests[drop:])]
			}
			if len(logs) > maxBufferedLogs {
				drop := len(logs) - maxBufferedLogs
				logx.Warnf("logstore: dropping %d log rows after repeated insert failures", drop)
				logs = logs[:copy(logs, logs[drop:])]
			}
			return
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
		statement, err := tx.Prepare(`INSERT INTO requests (ts, method, host, path, status, duration_ms, bytes_out, ip, ua, request_id, process) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		for _, e := range requests {
			if _, err := statement.Exec(stamp(e.Time), e.Method, e.Host, e.Path, e.Status, e.DurationMS, e.BytesOut, e.IP, e.UserAgent, e.RequestID, e.Process); err != nil {
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
	channelProcess := false
	if filter.Channel != "" {
		if path, ok := strings.CutPrefix(filter.Channel, "file:"); ok {
			where = append(where, "logs.source = 'file'", "logs.process = ?")
			args = append(args, path)
			channelProcess = true
		} else if name, ok := strings.CutPrefix(filter.Channel, "stdout:"); ok {
			where = append(where, "logs.source = 'stdout'", "logs.process = ?")
			args = append(args, name)
			channelProcess = true
		} else {
			where = append(where, "logs.source = ?")
			args = append(args, filter.Channel)
		}
	}
	if filter.Process != "" && !channelProcess {
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
	if !filter.Before.IsZero() {
		where = append(where, "logs.ts < ?")
		args = append(args, stamp(filter.Before))
	}
	from := "logs"
	if term := ftsQuery(filter.Query); term != "" {
		from = "logs JOIN logs_fts ON logs_fts.rowid = logs.rowid"
		where = append(where, "logs_fts MATCH ?")
		args = append(args, term)
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
	if filter.Method != "" {
		where = append(where, "method = ?")
		args = append(args, filter.Method)
	}
	if filter.Process != "" {
		where = append(where, "process = ?")
		args = append(args, filter.Process)
	}
	if filter.Status != 0 {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.StatusClass != 0 {
		where = append(where, "status >= ? AND status < ?")
		args = append(args, filter.StatusClass, filter.StatusClass+100)
	}
	if !filter.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, stamp(filter.Since))
	}
	if !filter.Before.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, stamp(filter.Before))
	}
	if filter.Query != "" {
		where = append(where, "(host LIKE ? OR path LIKE ? OR ip LIKE ? OR ua LIKE ?)")
		like := "%" + filter.Query + "%"
		args = append(args, like, like, like, like)
	}
	query := `SELECT ts, method, host, path, status, duration_ms, bytes_out, ip, ua, request_id, process FROM requests`
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
		if err := rows.Scan(&ts, &e.Method, &e.Host, &e.Path, &e.Status, &e.DurationMS, &e.BytesOut, &e.IP, &e.UserAgent, &e.RequestID, &e.Process); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, ts)
		result = append(result, e)
	}
	return result, rows.Err()
}

// Channels lists the log types an app has. The reserved HostApp exposes only the appboss daemon
// log; every real app exposes the request table, one channel per service that wrote stdout, and one
// channel per app log file.
func (s *Store) Channels(app string) ([]Channel, error) {
	return s.channelsFor(app)
}

// RecordAudit writes one operator action to the reserved host database. Audit rows are not
// batched: the volume is tiny and the console reads them right after the action.
func (s *Store) RecordAudit(e AuditEntry) error {
	w, err := s.writer(HostApp)
	if err != nil {
		return err
	}
	_, err = w.db.Exec(`INSERT INTO audit (ts, actor, app, action, detail, result, error) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		stamp(e.Time), e.Actor, e.App, e.Action, e.Detail, e.Result, e.Error)
	return err
}

// SearchAudit returns the newest matching audit rows, newest first.
func (s *Store) SearchAudit(filter AuditFilter) ([]AuditEntry, error) {
	w, err := s.writer(HostApp)
	if err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	where := []string{}
	args := []any{}
	for column, value := range map[string]string{"app": filter.App, "actor": filter.Actor, "action": filter.Action} {
		if value != "" {
			where = append(where, column+" = ?")
			args = append(args, value)
		}
	}
	if !filter.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, stamp(filter.Since))
	}
	if !filter.Before.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, stamp(filter.Before))
	}
	query := `SELECT ts, actor, app, action, detail, result, error FROM audit`
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
	var result []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts string
		if err := rows.Scan(&ts, &e.Actor, &e.App, &e.Action, &e.Detail, &e.Result, &e.Error); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, ts)
		result = append(result, e)
	}
	return result, rows.Err()
}

// Tree returns name, on-disk sqlite size and channels for each app. Missing databases stay
// missing: this does not create them.
func (s *Store) Tree(names []string) ([]AppTree, error) {
	out := make([]AppTree, 0, len(names))
	for _, name := range names {
		channels, err := s.channelsFor(name)
		if err != nil {
			return nil, err
		}
		out = append(out, AppTree{Name: name, Bytes: s.diskBytes(name), Channels: channels})
	}
	return out, nil
}

func (s *Store) channelsFor(app string) ([]Channel, error) {
	if app == HostApp {
		return []Channel{{ID: "appboss", Label: "appboss"}}, nil
	}
	channels := []Channel{}
	requesters, err := s.distinct(app, `SELECT DISTINCT process FROM requests WHERE process <> '' ORDER BY process`)
	if err != nil {
		return nil, err
	}
	for _, name := range requesters {
		channels = append(channels, Channel{ID: "request:" + name, Label: name + " requests"})
	}
	services, err := s.distinct(app, `SELECT DISTINCT process FROM logs WHERE source = 'stdout' ORDER BY process`)
	if err != nil {
		return nil, err
	}
	for _, name := range services {
		channels = append(channels, Channel{ID: "stdout:" + name, Label: name})
	}
	files, err := s.distinct(app, `SELECT DISTINCT process FROM logs WHERE source = 'file' ORDER BY process`)
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		channels = append(channels, Channel{ID: "file:" + path, Label: path})
	}
	return channels, nil
}

func (s *Store) diskBytes(app string) int64 {
	var total int64
	dir := filepath.Join(s.dir, app)
	for _, name := range []string{"appboss.sqlite", "appboss.sqlite-wal", "appboss.sqlite-shm"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}

// distinct runs a single-column query against the app database, without creating one.
func (s *Store) distinct(app, query string) ([]string, error) {
	s.mu.Lock()
	w := s.apps[app]
	s.mu.Unlock()
	if w != nil {
		return queryValues(w.db, query)
	}
	path := filepath.Join(s.dir, app, "appboss.sqlite")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return queryValues(db, query)
}

func queryValues(db *sql.DB, query string) ([]string, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// TailOffsets returns every tracked app log file for app, keyed by absolute path.
func (s *Store) TailOffsets(app string) (map[string]TailOffset, error) {
	w, err := s.writer(app)
	if err != nil {
		return nil, err
	}
	rows, err := w.db.Query(`SELECT path, inode, offset FROM tail_offsets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]TailOffset{}
	for rows.Next() {
		var offset TailOffset
		if err := rows.Scan(&offset.Path, &offset.Inode, &offset.Offset); err != nil {
			return nil, err
		}
		result[offset.Path] = offset
	}
	return result, rows.Err()
}

// SaveTailOffset records how far the tailer read into one app log file.
func (s *Store) SaveTailOffset(app, path string, inode uint64, offset int64) error {
	w, err := s.writer(app)
	if err != nil {
		return err
	}
	_, err = w.db.Exec(`INSERT INTO tail_offsets (path, inode, offset, updated_ts) VALUES (?, ?, ?, ?) ON CONFLICT(path) DO UPDATE SET inode = excluded.inode, offset = excluded.offset, updated_ts = excluded.updated_ts`, path, inode, offset, stamp(time.Now()))
	return err
}

// RemoveTailOffsets drops the tracked offsets of files that no longer exist.
func (s *Store) RemoveTailOffsets(app string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	w, err := s.writer(app)
	if err != nil {
		return err
	}
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}
	for _, path := range paths {
		if _, err := tx.Exec(`DELETE FROM tail_offsets WHERE path = ?`, path); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
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

// Latency computes duration quantiles for requests newer than since. It reads the newest rows up
// to latencySampleLimit and sorts them in memory.
func (s *Store) Latency(app string, since time.Time) (Latency, error) {
	w, err := s.writer(app)
	if err != nil {
		return Latency{}, err
	}
	rows, err := w.db.Query(`SELECT duration_ms FROM requests WHERE ts >= ? ORDER BY ts DESC LIMIT ?`, stamp(since), latencySampleLimit)
	if err != nil {
		return Latency{}, err
	}
	defer rows.Close()
	durations := make([]int64, 0, 1024)
	for rows.Next() {
		var duration int64
		if err := rows.Scan(&duration); err != nil {
			return Latency{}, err
		}
		durations = append(durations, duration)
	}
	if err := rows.Err(); err != nil {
		return Latency{}, err
	}
	slices.Sort(durations)
	return Latency{Count: len(durations), P50: percentile(durations, 0.5), P95: percentile(durations, 0.95), P99: percentile(durations, 0.99)}, nil
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

// Prune deletes rows older than their retention from one app's database. Request rows and app
// log files use retention, process stdout and the appboss daemon log use stdoutRetention. A zero
// retention disables the whole store, matching Request/RecordLogs.
func (s *Store) Prune(ctx context.Context, app string, retention, stdoutRetention time.Duration) error {
	w, err := s.writerForPrune(app)
	if err != nil {
		return err
	}
	if w == nil {
		return nil
	}
	// Audit rows live in the host database and use their own retention, independent of the log
	// retention that may be disabled for the app.
	if app == HostApp && s.auditRetention > 0 {
		if _, err := w.db.ExecContext(ctx, `DELETE FROM audit WHERE ts < ?`, stamp(time.Now().Add(-s.auditRetention))); err != nil {
			return err
		}
	}
	if retention <= 0 {
		return nil
	}
	if _, err := w.db.ExecContext(ctx, `DELETE FROM requests WHERE ts < ?`, stamp(time.Now().Add(-retention))); err != nil {
		return err
	}
	if _, err := w.db.ExecContext(ctx, `DELETE FROM logs WHERE source = 'file' AND ts < ?`, stamp(time.Now().Add(-retention))); err != nil {
		return err
	}
	if stdoutRetention > 0 {
		cutoff := stamp(time.Now().Add(-stdoutRetention))
		// Everything that is not an app log file is the short-lived console stream, including
		// legacy rows written before the channels existed.
		if _, err := w.db.ExecContext(ctx, `DELETE FROM logs WHERE source <> 'file' AND ts < ?`, cutoff); err != nil {
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
			known := map[string]bool{}
			for _, snapshot := range s.snapshotter.Snapshots() {
				known[snapshot.Name] = true
				if err := s.Prune(s.ctx, snapshot.Name, snapshot.LogRetention, snapshot.StdoutRetention); err != nil {
					logx.Warnf("log prune %s: %v", snapshot.Name, err)
				}
			}
			// Databases left behind by apps removed from the config keep the host retention so
			// they cannot grow forever after removal.
			for _, app := range s.diskApps() {
				if known[app] || app == HostApp {
					continue
				}
				if err := s.Prune(s.ctx, app, s.hostRetention, s.hostRetention); err != nil {
					logx.Warnf("log prune %s: %v", app, err)
				}
			}
			if err := s.Prune(s.ctx, HostApp, s.hostRetention, s.hostRetention); err != nil {
				logx.Warnf("log prune %s: %v", HostApp, err)
			}
		}
	}
}

// Vacuum rewrites one app's database to reclaim the space the prune freed. A missing database is
// a no-op.
func (s *Store) Vacuum(ctx context.Context, app string) error {
	s.mu.Lock()
	w := s.apps[app]
	s.mu.Unlock()
	if w != nil {
		_, err := w.db.ExecContext(ctx, `VACUUM`)
		return err
	}
	path := filepath.Join(s.dir, app, "appboss.sqlite")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `VACUUM`)
	return err
}

func (s *Store) vacuumLoop() {
	for {
		now := time.Now()
		target, _ := time.ParseInLocation("15:04", s.vacuumAt, now.Location())
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
			// Vacuum every database on disk, including databases left by removed apps.
			for _, app := range s.diskApps() {
				if app == HostApp {
					continue
				}
				if err := s.Vacuum(s.ctx, app); err != nil {
					logx.Warnf("log vacuum %s: %v", app, err)
				}
			}
			if err := s.Vacuum(s.ctx, HostApp); err != nil {
				logx.Warnf("log vacuum %s: %v", HostApp, err)
			}
		}
	}
}
