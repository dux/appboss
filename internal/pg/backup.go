package pg

import (
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

// globalsDatabase is the synthetic name the cluster globals dump is cataloged under.
const globalsDatabase = "_globals"

// BackupResult is the outcome of one dump, returned to the console and CLI.
type BackupResult struct {
	Database   string `json:"database"`
	Globals    bool   `json:"globals,omitempty"`
	Status     string `json:"status"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// Backups lists every recorded dump, newest first.
func (s *Service) Backups() []Backup { return s.catalog.list() }

// BackupAll dumps every selected database and, when enabled, the cluster globals. It returns the
// first error but always attempts every database, so one failure does not skip the rest.
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
	if len(selected) == 0 && !postgres.Backup.Globals {
		return errors.New("no databases are selected for backup")
	}
	var firstErr error
	for _, database := range selected {
		if _, err := s.BackupDatabase(ctx, database); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if postgres.Backup.Globals {
		if _, err := s.backupGlobals(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.prune(ctx)
	return firstErr
}

// BackupDatabase dumps one database in custom format, stores it where configured and records the
// result. A second call for a database already running is refused.
func (s *Service) BackupDatabase(ctx context.Context, database string) (Backup, error) {
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
	local, wants3 := opts.postgres.Backup.Destinations(database)
	return s.runDump(ctx, connConfig, opts, database, false, local, wants3)
}

func (s *Service) backupGlobals(ctx context.Context) (Backup, error) {
	s.mu.Lock()
	if s.running[globalsDatabase] {
		s.mu.Unlock()
		return Backup{}, errors.New("a globals backup is already running")
	}
	s.running[globalsDatabase] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, globalsDatabase)
		s.mu.Unlock()
	}()

	s.mu.RLock()
	opts, connConfig := s.opts, s.connConfig
	local, wants3 := opts.postgres.Backup.Destinations(globalsDatabase)
	s.mu.RUnlock()
	if connConfig == nil {
		return Backup{}, errors.New("no reachable PostgreSQL server")
	}
	return s.runDump(ctx, connConfig, opts, globalsDatabase, true, local, wants3)
}

// runDump is the shared path for a database or the globals set: create a temp file, run pg_dump,
// checksum it, move it into place, upload it and record the result.
func (s *Service) runDump(ctx context.Context, connConfig *pgx.ConnConfig, opts options, database string, globals, local, wants3 bool) (Backup, error) {
	started := time.Now()
	name := dumpName(database, globals, started)
	entry := Backup{ID: name, Database: database, Time: started.UTC().Format(time.RFC3339), Status: "ok", Globals: globals}

	tmpDir, err := s.dumpDir(opts, database, local)
	if err != nil {
		return s.failed(entry, started, err)
	}
	temp, err := os.CreateTemp(tmpDir, "."+name+".*.tmp")
	if err != nil {
		return s.failed(entry, started, err)
	}
	tempPath := temp.Name()
	_ = temp.Close()
	defer func() { _ = os.Remove(tempPath) }()

	if err := s.execDump(ctx, connConfig, opts, database, globals, tempPath); err != nil {
		return s.failed(entry, started, err)
	}
	size, err := fileSize(tempPath)
	if err != nil {
		return s.failed(entry, started, err)
	}
	sum, err := fileSHA256(tempPath)
	if err != nil {
		return s.failed(entry, started, err)
	}
	entry.Bytes, entry.SHA256 = size, sum

	finalPath := tempPath
	if local {
		finalPath = filepath.Join(filepath.Dir(tempPath), name)
		if err := os.Rename(tempPath, finalPath); err != nil {
			return s.failed(entry, started, err)
		}
		entry.LocalPath = finalPath
	}
	if wants3 {
		uploader, err := newS3(opts.s3)
		if err != nil {
			return s.failed(entry, started, err)
		}
		if uploader == nil {
			return s.failed(entry, started, errors.New("s3 is not configured"))
		}
		key := objectKey(opts.s3.Prefix, database, globals, name)
		if err := uploader.Put(ctx, key, finalPath, size); err != nil {
			return s.failed(entry, started, err)
		}
		entry.S3Key = key
	}
	if !local {
		_ = os.Remove(finalPath)
	}
	entry.DurationMS = time.Since(started).Milliseconds()
	if err := s.catalog.record(entry); err != nil {
		return entry, err
	}
	logx.Infof("postgres backup: %s %s %s", database, humanBytes(size), started.UTC().Format(time.RFC3339))
	return entry, nil
}

// dumpDir is where the temp file lives: next to the final local copy when one is requested, else
// the system temp directory for an upload-only backup.
func (s *Service) dumpDir(opts options, database string, local bool) (string, error) {
	if local {
		dir := filepath.Join(opts.postgres.Backup.Dir, database)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", err
		}
		return dir, nil
	}
	return os.TempDir(), nil
}

func (s *Service) execDump(ctx context.Context, connConfig *pgx.ConnConfig, opts options, database string, globals bool, path string) error {
	runCtx := ctx
	if timeout := opts.postgres.Backup.Timeout.Value(); timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var command *exec.Cmd
	if globals {
		command = exec.CommandContext(runCtx, "pg_dumpall", "--globals-only", "--file="+path, "--dbname="+serverConnString(connConfig))
	} else {
		command = exec.CommandContext(runCtx, "pg_dump", "--format=custom", "--compress=6", "--no-owner", "--no-privileges", "--file="+path, "--dbname="+databaseConnString(connConfig, database))
	}
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

// failed records and notifies about a dump that did not land.
func (s *Service) failed(entry Backup, started time.Time, err error) (Backup, error) {
	entry.Status, entry.Error = "failed", err.Error()
	entry.DurationMS = time.Since(started).Milliseconds()
	_ = s.catalog.record(entry)
	s.sink.Send(notify.Event{Type: "backup-failed", App: entry.Database, Error: err.Error(), Time: time.Now()})
	return entry, err
}

// prune removes dumps beyond the retention policy from disk, S3 and the catalog. Failed entries
// are kept for visibility until the catalog cap drops them.
func (s *Service) prune(ctx context.Context) {
	s.mu.RLock()
	opts := s.opts
	s.mu.RUnlock()
	policy := Policy{
		Hourly:  opts.postgres.Backup.Keep.Hourly,
		Daily:   opts.postgres.Backup.Keep.Daily,
		Weekly:  opts.postgres.Backup.Keep.Weekly,
		Monthly: opts.postgres.Backup.Keep.Monthly,
	}
	entries := s.catalog.list()
	keep := keepSet(entries, policy)
	uploader, _ := newS3(opts.s3)
	removed := map[string]bool{}
	for _, entry := range entries {
		if entry.Status != "ok" || keep[entry.ID] {
			continue
		}
		if entry.LocalPath != "" {
			_ = os.Remove(entry.LocalPath)
		}
		if entry.S3Key != "" && uploader != nil {
			if err := uploader.Remove(ctx, entry.S3Key); err != nil {
				logx.Warnf("postgres prune: remove %s: %v", entry.S3Key, err)
			}
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

func dumpName(database string, globals bool, at time.Time) string {
	ext := ".dump"
	if globals {
		ext = ".sql"
	}
	return database + "-" + at.UTC().Format("2006-01-02T15-04-05Z") + ext
}

func objectKey(prefix, database string, globals bool, name string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	if globals {
		return prefix + globalsDatabase + "/" + name
	}
	return prefix + database + "/" + name
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
