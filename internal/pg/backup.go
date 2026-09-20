package pg

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"dboss/internal/logx"
	"dboss/internal/notify"

	"github.com/jackc/pgx/v5"
)

// Backups always land under <config dir>/pg_backup/<database>/ and follow one daily schedule; only
// the per-database rotation window is configurable.
const (
	backupDirName = "pg_backup"
	backupAt      = "04:00"
	backupTimeout = time.Hour
)

// Backups lists every recorded dump, newest first.
func (s *Service) Backups() []Backup { return s.catalog.list() }

// DeleteBackup removes one recorded dump from disk and the catalog.
func (s *Service) DeleteBackup(id string) error {
	entry, ok := s.catalog.get(id)
	if !ok {
		return fmt.Errorf("unknown backup %q", id)
	}
	if entry.LocalPath != "" {
		if err := os.Remove(entry.LocalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.catalog.forget(map[string]bool{id: true})
}

// BackupAll dumps every selected database. It returns the first error but always attempts every
// database, so one failure does not skip the rest.
func (s *Service) BackupAll(ctx context.Context) error {
	s.mu.RLock()
	postgres, connConfig := s.opts.postgres, s.connConfig
	s.mu.RUnlock()
	if !postgres.Enabled {
		return errors.New("postgres is disabled")
	}
	if connConfig == nil {
		return errors.New("no reachable PostgreSQL server")
	}
	selected := postgres.Backup.Selected()
	if len(selected) == 0 {
		return errors.New("no databases are selected for backup")
	}
	var firstErr error
	for _, database := range selected {
		if _, err := s.BackupDatabase(ctx, database, false); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.prune()
	return firstErr
}

// BackupDatabase dumps one database as a zip of a plain-SQL pg_dump. manual marks the entry as
// exempt from rotation. A second call for a database already running is refused.
func (s *Service) BackupDatabase(ctx context.Context, database string, manual bool) (Backup, error) {
	s.mu.Lock()
	if s.running[database] {
		s.mu.Unlock()
		return Backup{}, fmt.Errorf("%s: a backup is already running", database)
	}
	s.running[database] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, database)
		s.mu.Unlock()
	}()

	s.mu.RLock()
	opts, connConfig := s.opts, s.connConfig
	s.mu.RUnlock()
	if connConfig == nil {
		return Backup{}, errors.New("no reachable PostgreSQL server")
	}
	return s.runDump(ctx, connConfig, opts, database, manual)
}

// runDump is the shared path: dump plain SQL to a temp file, zip it into place, checksum the
// archive and record the result.
func (s *Service) runDump(ctx context.Context, connConfig *pgx.ConnConfig, opts options, database string, manual bool) (Backup, error) {
	started := time.Now()
	name := dumpName(database, started)
	entry := Backup{ID: name, Database: database, Time: started.UTC().Format(time.RFC3339), Status: "ok", Manual: manual}

	dir, err := s.dumpDir(opts, database)
	if err != nil {
		return s.failed(entry, started, err)
	}
	temp, err := os.CreateTemp(dir, ".dump-*.sql")
	if err != nil {
		return s.failed(entry, started, err)
	}
	sqlPath := temp.Name()
	_ = temp.Close()
	defer func() { _ = os.Remove(sqlPath) }()

	if err := s.execDump(ctx, connConfig, database, sqlPath); err != nil {
		return s.failed(entry, started, err)
	}
	finalPath := filepath.Join(dir, name)
	if err := zipSQL(sqlPath, finalPath); err != nil {
		return s.failed(entry, started, err)
	}
	size, err := fileSize(finalPath)
	if err != nil {
		return s.failed(entry, started, err)
	}
	sum, err := fileSHA256(finalPath)
	if err != nil {
		return s.failed(entry, started, err)
	}
	entry.Bytes, entry.SHA256, entry.LocalPath = size, sum, finalPath
	entry.DurationMS = time.Since(started).Milliseconds()
	if err := s.catalog.record(entry); err != nil {
		return entry, err
	}
	logx.Infof("postgres backup: %s %s %s", database, humanBytes(size), started.UTC().Format(time.RFC3339))
	return entry, nil
}

// dumpDir is the per-database directory the dump lives in: <config dir>/pg_backup/<database>.
func (s *Service) dumpDir(opts options, database string) (string, error) {
	dir := filepath.Join(opts.dir, backupDirName, database)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return dir, nil
}

// execDump runs pg_dump as plain SQL into path. The dump keeps schema, data, functions, triggers
// and core objects, without the ownership, ACLs, comments, tablespace or replication metadata that
// would not restore on another box. The connection password travels in the environment, never
// argv.
func (s *Service) execDump(ctx context.Context, connConfig *pgx.ConnConfig, database, path string) error {
	runCtx, cancel := context.WithTimeout(ctx, backupTimeout)
	defer cancel()
	command := exec.CommandContext(runCtx, "pg_dump",
		"--format=plain",
		"--no-owner", "--no-privileges",
		"--no-comments", "--no-tablespaces", "--no-security-labels",
		"--no-publications", "--no-subscriptions", "--no-table-access-method",
		"--file="+path,
		"--dbname="+databaseConnString(connConfig, database),
	)
	command.Env = processEnv(connConfig)
	output, err := command.CombinedOutput()
	if err != nil {
		message := firstLine(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("%s: %s", filepath.Base(command.Path), message)
	}
	return nil
}

// zipSQL stores the SQL dump at source in target as a zip archive. The archive holds one entry
// named after the dump file.
func zipSQL(source, target string) error {
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(out)
	entry, err := writer.Create(strings.TrimSuffix(filepath.Base(target), ".zip") + ".sql")
	if err != nil {
		_ = out.Close()
		return err
	}
	src, err := os.Open(source)
	if err != nil {
		_ = out.Close()
		return err
	}
	_, copyErr := io.Copy(entry, src)
	_ = src.Close()
	if copyErr != nil {
		_ = out.Close()
		return copyErr
	}
	if err := writer.Close(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// failed records and notifies about a dump that did not land.
func (s *Service) failed(entry Backup, started time.Time, err error) (Backup, error) {
	entry.Status, entry.Error = "failed", err.Error()
	entry.DurationMS = time.Since(started).Milliseconds()
	_ = s.catalog.record(entry)
	s.sink.Send(notify.Event{Type: "backup-failed", App: entry.Database, Error: err.Error(), Time: time.Now()})
	return entry, err
}

// prune removes scheduled dumps beyond their database's rotation window from disk and the catalog.
// Manual dumps and failed entries are kept for visibility until the catalog cap drops them.
func (s *Service) prune() {
	s.mu.RLock()
	opts := s.opts
	s.mu.RUnlock()
	now := time.Now()
	entries := s.catalog.list()
	byDatabase := map[string][]Backup{}
	for _, entry := range entries {
		byDatabase[entry.Database] = append(byDatabase[entry.Database], entry)
	}
	keep := map[string]bool{}
	for database, group := range byDatabase {
		window := rotationWindow(opts.postgres.Backup.Rotation(database))
		for id := range keepSet(group, window, now) {
			keep[id] = true
		}
	}
	removed := map[string]bool{}
	for _, entry := range entries {
		if entry.Status != "ok" || keep[entry.ID] {
			continue
		}
		if entry.LocalPath != "" {
			_ = os.Remove(entry.LocalPath)
		}
		removed[entry.ID] = true
	}
	if err := s.catalog.forget(removed); err != nil {
		logx.Warnf("postgres prune: catalog: %v", err)
	}
	if len(removed) > 0 {
		logx.Infof("postgres prune: removed %d old backup(s)", len(removed))
	}
}

func dumpName(database string, at time.Time) string {
	return "BACKUP_" + at.UTC().Format("2006-01-02T15-04-05Z") + ".zip"
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// humanBytes renders a size for the daemon log.
func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%dB", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(size)/float64(div), "KMGT"[exp])
}
