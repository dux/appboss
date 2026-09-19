package pg

import (
	"context"
	"sync"
	"time"

	"app-boss/internal/config"
	"app-boss/internal/logx"
	"app-boss/internal/notify"

	"github.com/jackc/pgx/v5"
)

const (
	// defaultInterval is how often the server facts are refreshed, matching the sysinfo module.
	defaultInterval = 30 * time.Second
	// backupTick is how often the scheduler checks for a due backup. It bounds how late a run can be.
	backupTick = 30 * time.Second
)

// Service owns PostgreSQL inspection and scheduled backups for one host session. It implements
// module.Module, so the daemon registers it once and drives the console and CLI from the same
// value.
type Service struct {
	sink notify.Sink

	mu         sync.RWMutex
	opts       options
	connConfig *pgx.ConnConfig
	snapshot   Snapshot
	interval   time.Duration
	next       time.Time

	refreshMu sync.Mutex

	catalog *catalog

	running         map[string]bool
	lastDetectError string

	baseCtx context.Context
	cancel  context.CancelFunc
	done    chan struct{}
}

// New builds the service. A nil sink disables backup notifications.
func New(cfg config.Config, sink notify.Sink) *Service {
	if sink == nil {
		sink = discardSink{}
	}
	return &Service{sink: sink, opts: optionsFrom(cfg), catalog: newCatalog(cfg.StateDir), running: map[string]bool{}, done: make(chan struct{})}
}

type discardSink struct{}

func (discardSink) Send(notify.Event) {}

func (s *Service) Name() string { return "postgres" }

func (s *Service) Start(ctx context.Context) error {
	ctx, s.cancel = context.WithCancel(ctx)
	s.mu.Lock()
	s.baseCtx = ctx
	s.mu.Unlock()
	// Detection talks to the network, so it runs off the start path and never delays the daemon.
	s.ApplyConfig()
	go s.infoLoop(ctx)
	go s.backupLoop(ctx)
	return nil
}

func (s *Service) Close() error {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
	return nil
}

// ApplyConfig re-reads the connection and backup policy. Called at start and after a config save,
// so the console can change the selection without a daemon restart. Detection runs in the
// background, so a slow or missing server never blocks the caller.
func (s *Service) ApplyConfig() {
	s.mu.Lock()
	interval := s.opts.postgres.Backup.Every.Value()
	switch {
	case interval <= 0:
		s.interval, s.next = 0, time.Time{}
	case interval != s.interval || s.next.IsZero():
		// A new interval restarts the clock, so a change takes effect from now.
		s.interval, s.next = interval, time.Now().Add(interval)
	}
	ctx := s.baseCtx
	s.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		s.detect(ctx)
		s.Refresh(ctx)
	}()
}

// Apply replaces the running configuration and re-resolves the connection.
func (s *Service) Apply(cfg config.Config) {
	s.mu.Lock()
	s.opts = optionsFrom(cfg)
	s.connConfig = nil
	s.mu.Unlock()
	s.catalog = newCatalog(cfg.StateDir)
	s.ApplyConfig()
}

// Enabled reports whether the operator has turned the feature on.
func (s *Service) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.opts.postgres.Enabled
}

// BackupConfig returns the effective backup policy for the console.
func (s *Service) BackupConfig() config.PostgresBackup {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.opts.postgres.Backup
}

// S3Configured reports whether dumps can be uploaded.
func (s *Service) S3Configured() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.opts.s3.Configured()
}

// Available reports whether the last inspection reached a server.
func (s *Service) Available() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot.Available
}

// Snapshot returns the cached inspection without touching the server.
func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

// detect resolves a connection string and records availability. It never fails: an unreachable
// server is reported in the snapshot's Error, which keeps the console tab hidden.
func (s *Service) detect(ctx context.Context) {
	s.mu.RLock()
	enabled, dsn := s.opts.postgres.Enabled, s.opts.postgres.DSN
	s.mu.RUnlock()
	if !enabled {
		s.mu.Lock()
		s.snapshot = Snapshot{CollectedAt: time.Now(), Error: "postgres is disabled"}
		s.connConfig = nil
		s.mu.Unlock()
		return
	}
	connConfig, err := resolve(ctx, dsn)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil || connConfig == nil {
		message := "no reachable PostgreSQL server"
		if err != nil {
			message = err.Error()
		}
		s.snapshot = Snapshot{CollectedAt: time.Now(), Error: message}
		s.connConfig = nil
		// Once per distinct failure, so a hidden tab still leaves a trail in the daemon log.
		if message != s.lastDetectError {
			logx.Warnf("postgres: %s (set postgres.dsn or postgres.enabled: false)", message)
			s.lastDetectError = message
		}
		return
	}
	s.connConfig = connConfig
	s.lastDetectError = ""
}

// Refresh reconnects and re-runs the inspection. It returns the new snapshot.
func (s *Service) Refresh(ctx context.Context) Snapshot {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	s.mu.RLock()
	enabled, connConfig, backupConfig := s.opts.postgres.Enabled, s.connConfig, s.opts.postgres.Backup
	s.mu.RUnlock()
	if !enabled {
		snapshot := Snapshot{CollectedAt: time.Now(), Error: "postgres is disabled"}
		s.mu.Lock()
		s.snapshot = snapshot
		s.mu.Unlock()
		return snapshot
	}
	if connConfig == nil {
		s.mu.RLock()
		snapshot := s.snapshot
		s.mu.RUnlock()
		if snapshot.CollectedAt.IsZero() {
			snapshot = Snapshot{CollectedAt: time.Now(), Error: "no reachable PostgreSQL server"}
		}
		return snapshot
	}
	conn, err := connect(ctx, connConfig)
	if err != nil {
		snapshot := Snapshot{CollectedAt: time.Now(), Error: err.Error(), Server: Server{Description: describe(connConfig)}}
		s.mu.Lock()
		s.snapshot = snapshot
		s.mu.Unlock()
		return snapshot
	}
	defer func() { _ = conn.Close(ctx) }()
	snapshot := collect(ctx, conn, connConfig, backupConfig)
	s.mu.Lock()
	s.snapshot = snapshot
	s.mu.Unlock()
	return snapshot
}

func (s *Service) infoLoop(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(defaultInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Refresh(ctx)
		}
	}
}

func (s *Service) backupLoop(ctx context.Context) {
	ticker := time.NewTicker(backupTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.maybeBackup(ctx, time.Now())
		}
	}
}

// maybeBackup fires the scheduled run when one is due. The actual work runs off the loop.
func (s *Service) maybeBackup(ctx context.Context, now time.Time) {
	s.mu.Lock()
	interval, next := s.interval, s.next
	if interval <= 0 || next.IsZero() || now.Before(next) {
		s.mu.Unlock()
		return
	}
	s.next = now.Add(interval)
	s.mu.Unlock()
	go func() {
		if err := s.BackupAll(ctx); err != nil {
			logx.Warnf("postgres backup: %v", err)
		}
	}()
}
