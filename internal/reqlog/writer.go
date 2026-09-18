package reqlog

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

type Entry struct {
	Time       time.Time
	Method     string
	Host       string
	Path       string
	Status     int
	DurationMS int64
	BytesOut   int64
	IP         string
	UserAgent  string
	RequestID  string
}

type Rates struct {
	LastMinute int64
	LastHour   int64
	LastDay    int64
}

type Manager struct {
	logDir  string
	flush   time.Duration
	mu      sync.Mutex
	writers map[string]*writer
}

type writer struct {
	db        *sql.DB
	entries   chan Entry
	retention atomic.Int64
	stop      chan struct{}
	done      chan struct{}
}

func New(logDir string, flush time.Duration) *Manager {
	return &Manager{logDir: logDir, flush: flush, writers: map[string]*writer{}}
}

func (m *Manager) Record(app string, retention, flush time.Duration, entry Entry) error {
	if retention <= 0 {
		return nil
	}
	w, err := m.get(app, retention, flush)
	if err != nil {
		return err
	}
	select {
	case w.entries <- entry:
		return nil
	default:
		return fmt.Errorf("request log queue full for %s", app)
	}
}

func (m *Manager) get(app string, retention, flush time.Duration) (*writer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := m.writers[app]; w != nil {
		w.retention.Store(int64(retention))
		return w, nil
	}
	dir := filepath.Join(m.logDir, app)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "requests.sqlite"))
	if err != nil {
		return nil, err
	}
	for _, statement := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS requests (ts TEXT NOT NULL, method TEXT NOT NULL, host TEXT NOT NULL, path TEXT NOT NULL, status INTEGER NOT NULL, duration_ms INTEGER NOT NULL, bytes_out INTEGER NOT NULL, ip TEXT NOT NULL, ua TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS requests_ts ON requests(ts)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := addColumn(db, "request_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, err
	}
	w := &writer{db: db, entries: make(chan Entry, 2048), stop: make(chan struct{}), done: make(chan struct{})}
	w.retention.Store(int64(retention))
	m.writers[app] = w
	if flush <= 0 {
		flush = m.flush
	}
	go w.loop(flush)
	return w, nil
}

// addColumn is the whole migration story: databases created before a column existed get it on open.
func addColumn(db *sql.DB, name, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(requests)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, notNull, primaryKey int
			column, columnType      string
			defaultValue            sql.NullString
		)
		if err := rows.Scan(&id, &column, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if column == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE requests ADD COLUMN ` + name + ` ` + definition)
	return err
}

func (m *Manager) Prune(ctx context.Context) error {
	m.mu.Lock()
	writers := make([]*writer, 0, len(m.writers))
	for _, w := range m.writers {
		writers = append(writers, w)
	}
	m.mu.Unlock()
	for _, w := range writers {
		retention := time.Duration(w.retention.Load())
		if _, err := w.db.ExecContext(ctx, `DELETE FROM requests WHERE ts < ?`, time.Now().Add(-retention).UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) PruneApp(ctx context.Context, app string, retention time.Duration) error {
	if retention <= 0 {
		return nil
	}
	path := filepath.Join(m.logDir, app, "requests.sqlite")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `DELETE FROM requests WHERE ts < ?`, time.Now().Add(-retention).UTC().Format(time.RFC3339Nano))
	return err
}

func (m *Manager) Rates(app string) (Rates, error) {
	path := filepath.Join(m.logDir, app, "requests.sqlite")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return Rates{}, nil
		}
		return Rates{}, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return Rates{}, err
	}
	defer db.Close()
	now := time.Now().UTC()
	var rates Rates
	for cutoff, target := range map[time.Duration]*int64{time.Minute: &rates.LastMinute, time.Hour: &rates.LastHour, 24 * time.Hour: &rates.LastDay} {
		if err := db.QueryRow(`SELECT count(*) FROM requests WHERE ts >= ?`, now.Add(-cutoff).Format(time.RFC3339Nano)).Scan(target); err != nil {
			return Rates{}, err
		}
	}
	return rates, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	writers := make([]*writer, 0, len(m.writers))
	for _, w := range m.writers {
		writers = append(writers, w)
	}
	m.mu.Unlock()
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

func (w *writer) loop(flush time.Duration) {
	defer close(w.done)
	ticker := time.NewTicker(flush)
	defer ticker.Stop()
	batch := make([]Entry, 0, 256)
	for {
		select {
		case entry := <-w.entries:
			batch = append(batch, entry)
			if len(batch) >= 256 {
				w.insert(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			w.insert(batch)
			batch = batch[:0]
		case <-w.stop:
			for {
				select {
				case entry := <-w.entries:
					batch = append(batch, entry)
				default:
					w.insert(batch)
					return
				}
			}
		}
	}
}

func (w *writer) insert(entries []Entry) {
	if len(entries) == 0 {
		return
	}
	tx, err := w.db.Begin()
	if err != nil {
		return
	}
	statement, err := tx.Prepare(`INSERT INTO requests (ts, method, host, path, status, duration_ms, bytes_out, ip, ua, request_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return
	}
	defer statement.Close()
	for _, entry := range entries {
		if _, err := statement.Exec(entry.Time.UTC().Format(time.RFC3339Nano), entry.Method, entry.Host, entry.Path, entry.Status, entry.DurationMS, entry.BytesOut, entry.IP, entry.UserAgent, entry.RequestID); err != nil {
			_ = tx.Rollback()
			return
		}
	}
	_ = tx.Commit()
}
