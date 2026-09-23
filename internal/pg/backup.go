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

// BackupFile returns a recorded dump and its path on disk, for the console download. The archive
// is handed out exactly as it was written, so it can be uploaded to another host.
func (s *Service) BackupFile(id string) (Backup, string, error) {
	entry, ok := s.catalog.get(id)
	if !ok {
		return Backup{}, "", fmt.Errorf("unknown backup %q", id)
	}
	if entry.Status != "ok" {
		return Backup{}, "", fmt.Errorf("backup %q did not complete", id)
	}
	path, err := fetch(entry)
	if err != nil {
		return Backup{}, "", err
	}
	return entry, path, nil
}

// ImportBackup stores an archive an operator uploaded. The bytes land verbatim under the
// database's backup directory and the entry is recorded as manual, so it restores through the
// normal path and rotation never removes it.
func (s *Service) ImportBackup(database string, source io.Reader) (Backup, error) {
	if err := validDatabaseName(database); err != nil {
		return Backup{}, err
	}
	s.mu.RLock()
	opts := s.opts
	s.mu.RUnlock()

	dir, err := s.dumpDir(opts, database)
	if err != nil {
		return Backup{}, err
	}
	temp, err := os.CreateTemp(dir, ".upload-*.zip")
	if err != nil {
		return Backup{}, err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	_, copyErr := io.Copy(temp, source)
	if closeErr := temp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return Backup{}, copyErr
	}
	if err := readableArchive(tempPath); err != nil {
		return Backup{}, err
	}

	started := time.Now()
	name, finalPath := uniqueDump(dir, started)
	if err := os.Rename(tempPath, finalPath); err != nil {
		return Backup{}, err
	}
	size, err := fileSize(finalPath)
	if err != nil {
		return Backup{}, err
	}
	sum, err := fileSHA256(finalPath)
	if err != nil {
		return Backup{}, err
	}
	entry := Backup{
		ID: name, Database: database, Time: started.UTC().Format(time.RFC3339), Status: "ok", Manual: true,
		Bytes: size, SHA256: sum, LocalPath: finalPath,
	}
	if err := s.catalog.record(entry); err != nil {
		return entry, err
	}
	logx.Infof("postgres backup uploaded: %s %s %s", database, humanBytes(size), entry.Time)
	return entry, nil
}

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
	postgres := s.opts.postgres
	s.mu.RUnlock()
	if !postgres.Enabled {
		return errors.New("postgres is disabled")
	}
	if _, err := s.connection(ctx); err != nil {
		return err
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

	connConfig, err := s.connection(ctx)
	if err != nil {
		return Backup{}, err
	}
	s.mu.RLock()
	opts := s.opts
	s.mu.RUnlock()
	return s.runDump(ctx, connConfig, opts, database, manual)
}

// runDump is the shared path: dump plain SQL to a temp file, zip it into place, checksum the
// archive and record the result.
func (s *Service) runDump(ctx context.Context, connConfig *pgx.ConnConfig, opts options, database string, manual bool) (Backup, error) {
	started := time.Now()
	entry := Backup{Database: database, Time: started.UTC().Format(time.RFC3339), Status: "ok", Manual: manual}

	dir, err := s.dumpDir(opts, database)
	if err != nil {
		return s.failed(entry, started, err)
	}
	name, finalPath := uniqueDump(dir, started)
	entry.ID = name
	temp, err := os.CreateTemp(dir, ".dump-*.sql")
	if err != nil {
		return s.failed(entry, started, err)
	}
	sqlPath := temp.Name()
	_ = temp.Close()
	defer func() { _ = os.Remove(sqlPath) }()

	if err := execDump(ctx, connConfig, database, sqlPath); err != nil {
		return s.failed(entry, started, err)
	}
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
func execDump(ctx context.Context, connConfig *pgx.ConnConfig, database, path string) error {
	ctx, cancel := context.WithTimeout(ctx, backupTimeout)
	defer cancel()
	return runClient(ctx, connConfig, "pg_dump",
		"--format=plain",
		"--no-owner", "--no-privileges",
		"--no-comments", "--no-tablespaces", "--no-security-labels",
		"--no-publications", "--no-subscriptions", "--no-table-access-method",
		"--file="+path,
		"--dbname="+databaseConnString(connConfig, database),
	)
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
	s.sink.Send(notify.Event{Type: notify.BackupFailed, App: entry.Database, Error: err.Error(), Time: time.Now()})
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

func dumpName(at time.Time) string {
	return "BACKUP_" + at.UTC().Format("2006-01-02T15-04-05Z") + ".zip"
}

// uniqueDump names a dump and returns its path. The stamp is second-resolution, so a second dump
// within the same second takes a suffix rather than overwriting the first and stealing its id.
func uniqueDump(dir string, at time.Time) (string, string) {
	name := dumpName(at)
	path := filepath.Join(dir, name)
	for index := 2; ; index++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return name, path
		}
		name = fmt.Sprintf("%s_%d.zip", strings.TrimSuffix(dumpName(at), ".zip"), index)
		path = filepath.Join(dir, name)
	}
}

// validDatabaseName keeps an uploaded archive inside the backup directory. PostgreSQL identifiers
// stop at 63 bytes, so anything longer is a typo or an attempt at something else.
func validDatabaseName(database string) error {
	if database == "" {
		return errors.New("database name is required")
	}
	if len(database) > 63 || strings.ContainsAny(database, `/\`) || strings.Contains(database, "..") || strings.HasPrefix(database, ".") {
		return fmt.Errorf("%q is not a valid database name", database)
	}
	return nil
}

// readableArchive is the upload's integrity check: a truncated or mistyped file fails here rather
// than at restore time, when a target database is already on the line.
func readableArchive(path string) error {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("upload is not a readable zip archive: %w", err)
	}
	defer reader.Close()
	if len(reader.File) == 0 {
		return errors.New("upload is an empty zip archive")
	}
	return nil
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
