// Package logstore keeps every app's logs and request rows in one per-app SQLite database,
// batches writes in the background and prunes by retention. A ClickHouse or remote backend can
// implement the same surface later without touching the proxy, the supervisor or the console.
package logstore

import (
	"context"
	"os"
	"sync"
	"time"

	"dboss/internal/schedule"
	"dboss/internal/supervisor"

	_ "modernc.org/sqlite"
)

// Snapshotter is the piece of the supervisor the prune loop needs: the apps and their retention.
type Snapshotter interface {
	Snapshots() []supervisor.Snapshot
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

// New returns a store that writes under dir (one <app>/dboss.sqlite per app). snapshotter and
// pruneAt drive the daily retention prune; pass nil to disable it. vacuumAt schedules the daily
// VACUUM (empty disables it). hostRetention bounds the reserved HostApp database that holds
// dboss's own daemon log; auditRetention bounds the audit table (0 keeps audit rows forever).
func New(dir string, flush time.Duration, snapshotter Snapshotter, pruneAt, vacuumAt string, hostRetention, auditRetention time.Duration) *Store {
	return &Store{dir: dir, flush: flush, snapshotter: snapshotter, pruneAt: pruneAt, vacuumAt: vacuumAt, hostRetention: hostRetention, auditRetention: auditRetention, apps: map[string]*appWriter{}}
}

func (s *Store) Name() string { return "logstore" }

// Start launches the retention prune loop and the daily vacuum. Databases open lazily on first
// write.
func (s *Store) Start(ctx context.Context) error {
	s.ctx, s.cancel = context.WithCancel(ctx)
	if s.snapshotter != nil {
		go schedule.Daily(s.ctx, s.pruneAt, s.pruneAll)
		go schedule.Daily(s.ctx, s.vacuumAt, s.vacuumAll)
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
		if exists, _ := s.exists(entry.Name()); exists {
			names = append(names, entry.Name())
		}
	}
	return names
}
