package logstore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dboss/internal/logx"

	_ "modernc.org/sqlite"
)

const queueSize = 4096

type entry struct {
	request *RequestEntry
	logs    []LogEntry
	blocked string
}

type appWriter struct {
	db      *sql.DB
	entries chan entry
	stop    chan struct{}
	done    chan struct{}
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

// RecordBlocked counts a deny-blocked path in the reserved host database. The path is aggregated
// per flush, so the table grows with distinct paths, never with hits.
func (s *Store) RecordBlocked(path string) error {
	if len(path) > maxBlockedPath {
		path = strings.ToValidUTF8(path[:maxBlockedPath], "")
	}
	w, err := s.writer(HostApp)
	if err != nil {
		return err
	}
	select {
	case w.entries <- entry{blocked: path}:
		return nil
	default:
		return fmt.Errorf("blocked queue full for %s", HostApp)
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
	return w.insert(nil, entries, nil)
}

func (s *Store) writer(app string) (*appWriter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.apps[app]; w != nil {
		return w, nil
	}
	path := s.dbPath(app)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	for _, statement := range schema {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	// Databases written before requests carried a process or a country are missing the column; the
	// log cache is disposable but a failed insert would silently drop rows, so add it in place once.
	_, _ = db.Exec(`ALTER TABLE requests ADD COLUMN process TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE requests ADD COLUMN country TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE exceptions ADD COLUMN is_resolved INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE exceptions ADD COLUMN is_ignored INTEGER NOT NULL DEFAULT 0`)
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
		if exists, err := s.exists(app); !exists {
			return nil, err
		}
	}
	return s.writer(app)
}

var schema = []string{
	`PRAGMA journal_mode=WAL`,
	`PRAGMA busy_timeout=5000`,
	`CREATE TABLE IF NOT EXISTS requests (ts TEXT NOT NULL, method TEXT NOT NULL, host TEXT NOT NULL, path TEXT NOT NULL, status INTEGER NOT NULL, duration_ms INTEGER NOT NULL, bytes_out INTEGER NOT NULL, ip TEXT NOT NULL, ua TEXT NOT NULL, request_id TEXT NOT NULL DEFAULT '', process TEXT NOT NULL DEFAULT '', country TEXT NOT NULL DEFAULT '')`,
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
	`CREATE TABLE IF NOT EXISTS blocked (path TEXT PRIMARY KEY, count INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS exceptions (exp_uid TEXT PRIMARY KEY, dump TEXT, first_at INTEGER NOT NULL, last_at INTEGER NOT NULL, count INTEGER NOT NULL, is_resolved INTEGER NOT NULL DEFAULT 0, is_ignored INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS exception_logs (exp_uid TEXT NOT NULL REFERENCES exceptions(exp_uid), minute_at INTEGER NOT NULL, count INTEGER NOT NULL, message TEXT NOT NULL, users TEXT, tags TEXT, description TEXT, ips TEXT, PRIMARY KEY (exp_uid, minute_at))`,
	`CREATE INDEX IF NOT EXISTS exception_logs_minute ON exception_logs(minute_at)`,
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
	blocked := map[string]int{}
	flushNow := func() {
		if len(requests) == 0 && len(logs) == 0 && len(blocked) == 0 {
			return
		}
		if err := w.insert(requests, logs, blocked); err != nil {
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
			if len(blocked) > maxBufferedLogs {
				logx.Warnf("logstore: dropping %d blocked counters after repeated insert failures", len(blocked))
				blocked = map[string]int{}
			}
			return
		}
		requests, logs, blocked = requests[:0], logs[:0], map[string]int{}
	}
	drain := func(op entry) {
		if op.request != nil {
			requests = append(requests, *op.request)
		}
		if len(op.logs) > 0 {
			logs = append(logs, op.logs...)
		}
		if op.blocked != "" {
			blocked[op.blocked]++
		}
	}
	for {
		select {
		case op := <-w.entries:
			drain(op)
			if len(requests) >= 256 || len(blocked) >= 256 {
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

func (w *appWriter) insert(requests []RequestEntry, logs []LogEntry, blocked map[string]int) error {
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}
	if len(requests) > 0 {
		statement, err := tx.Prepare(`INSERT INTO requests (ts, method, host, path, status, duration_ms, bytes_out, ip, ua, request_id, process, country) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		for _, e := range requests {
			if _, err := statement.Exec(stamp(e.Time), e.Method, e.Host, e.Path, e.Status, e.DurationMS, e.BytesOut, e.IP, e.UserAgent, e.RequestID, e.Process, e.Country); err != nil {
				_ = statement.Close()
				_ = tx.Rollback()
				return err
			}
		}
		_ = statement.Close()
	}
	if len(logs) > 0 {
		if err := insertLogs(tx, logs); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if len(blocked) > 0 {
		statement, err := tx.Prepare(`INSERT INTO blocked (path, count) VALUES (?, ?) ON CONFLICT(path) DO UPDATE SET count = count + excluded.count`)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		for path, count := range blocked {
			if _, err := statement.Exec(path, count); err != nil {
				_ = statement.Close()
				_ = tx.Rollback()
				return err
			}
		}
		_ = statement.Close()
	}
	return tx.Commit()
}

// insertLogs writes process-log rows inside an open transaction, shared by the batched writer
// and the synchronous exception writer.
func insertLogs(tx *sql.Tx, entries []LogEntry) error {
	statement, err := tx.Prepare(`INSERT INTO logs (ts, source, process, stream, level, message, request_id, raw) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, e := range entries {
		if _, err := statement.Exec(stamp(e.Time), e.Source, e.Process, e.Stream, e.Level, e.Message, e.RequestID, e.Raw); err != nil {
			return err
		}
	}
	return nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

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
