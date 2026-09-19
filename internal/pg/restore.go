package pg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
)

// RestoreRequest selects a catalog entry and where to put it. Target defaults to a new database
// named <source>_restore_<timestamp>. Replacing an existing database requires Replace and a
// matching Confirm string.
type RestoreRequest struct {
	ID      string `json:"id"`
	Target  string `json:"target,omitempty"`
	Replace bool   `json:"replace,omitempty"`
	Confirm string `json:"confirm,omitempty"`
}

// RestoreResult reports the database that received the dump.
type RestoreResult struct {
	Target  string `json:"target"`
	Created bool   `json:"created"`
	Bytes   int64  `json:"bytes"`
}

// ErrRestoreConfirm is returned when an in-place restore is missing its confirmation.
var ErrRestoreConfirm = errors.New("restoring over an existing database requires the target name as confirmation")

// Restore verifies a dump and loads it into the target database. It never touches the source
// database unless Replace is set, and then only with an explicit confirmation.
func (s *Service) Restore(ctx context.Context, request RestoreRequest) (RestoreResult, error) {
	s.mu.RLock()
	opts, connConfig := s.opts, s.connConfig
	s.mu.RUnlock()
	if connConfig == nil {
		return RestoreResult{}, errors.New("no reachable PostgreSQL server")
	}
	entry, ok := s.catalog.get(request.ID)
	if !ok {
		return RestoreResult{}, fmt.Errorf("unknown backup %q", request.ID)
	}
	if entry.Status != "ok" {
		return RestoreResult{}, fmt.Errorf("backup %q did not complete", request.ID)
	}
	path, cleanup, err := s.fetch(ctx, opts, entry)
	if err != nil {
		return RestoreResult{}, err
	}
	defer cleanup()
	if entry.SHA256 != "" {
		if sum, err := fileSHA256(path); err != nil || sum != entry.SHA256 {
			return RestoreResult{}, fmt.Errorf("backup %q failed its checksum", request.ID)
		}
	}

	target := request.Target
	if target == "" {
		target = fmt.Sprintf("%s_restore_%s", entry.Database, time.Now().UTC().Format("20060102150405"))
	}
	if request.Replace {
		if request.Confirm != target {
			return RestoreResult{}, ErrRestoreConfirm
		}
	} else if err := s.createDatabase(ctx, connConfig, target); err != nil {
		return RestoreResult{}, err
	}
	if err := verifyDump(ctx, path, entry.Globals); err != nil {
		return RestoreResult{}, err
	}
	if err := runRestore(ctx, connConfig, entry, path, target, request.Replace); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{Target: target, Created: !request.Replace, Bytes: entry.Bytes}, nil
}

// fetch returns a local path to the dump, downloading from S3 when it is not on disk.
func (s *Service) fetch(ctx context.Context, opts options, entry Backup) (string, func(), error) {
	if entry.LocalPath != "" {
		if _, err := os.Stat(entry.LocalPath); err == nil {
			return entry.LocalPath, func() {}, nil
		}
	}
	if entry.S3Key == "" {
		return "", nil, fmt.Errorf("backup %q is not available locally or in s3", entry.ID)
	}
	uploader, err := newS3(opts.s3)
	if err != nil || uploader == nil {
		return "", nil, errors.New("s3 is not configured")
	}
	temp, err := os.CreateTemp("", "appboss-restore-*.dump")
	if err != nil {
		return "", nil, err
	}
	path := temp.Name()
	_ = temp.Close()
	if err := uploader.Get(ctx, entry.S3Key, path); err != nil {
		_ = os.Remove(path)
		return "", nil, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// createDatabase connects to the maintenance database and creates the target. It uses the first
// of postgres or template1 that accepts a connection.
func (s *Service) createDatabase(ctx context.Context, connConfig *pgx.ConnConfig, target string) error {
	var lastErr error
	for _, maintenance := range []string{"postgres", "template1"} {
		conn, err := pgx.ConnectConfig(ctx, mustConfig(connConfig, maintenance))
		if err != nil {
			lastErr = err
			continue
		}
		_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{target}.Sanitize())
		_ = conn.Close(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("create database %s: %w", target, lastErr)
}

func mustConfig(connConfig *pgx.ConnConfig, database string) *pgx.ConnConfig {
	clone := connConfig.Copy()
	clone.Database = database
	return clone
}

// verifyDump runs a cheap structural check so a truncated file fails before the target is
// touched. Plain-SQL globals dumps are not checked here.
func verifyDump(ctx context.Context, path string, globals bool) error {
	if globals {
		return nil
	}
	output, err := exec.CommandContext(ctx, "pg_restore", "--list", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("backup is not readable: %s", firstLine(string(output)))
	}
	return nil
}

func runRestore(ctx context.Context, connConfig *pgx.ConnConfig, entry Backup, path, target string, replace bool) error {
	var command *exec.Cmd
	if entry.Globals {
		command = exec.CommandContext(ctx, "psql", "--dbname="+databaseConnString(connConfig, target), "--file="+path)
	} else {
		args := []string{"--no-owner", "--no-privileges", "--dbname=" + databaseConnString(connConfig, target)}
		if replace {
			args = append(args, "--clean", "--if-exists")
		}
		args = append(args, path)
		command = exec.CommandContext(ctx, "pg_restore", args...)
	}
	command.Env = processEnv(connConfig)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", filepath.Base(command.Path), firstLine(string(output)))
	}
	return nil
}
