package pg

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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

// ErrDropConfirm is returned when a drop is missing the database name as confirmation.
var ErrDropConfirm = errors.New("dropping a database requires its name as confirmation")

// reservedDatabases can never be dropped through dboss.
var reservedDatabases = map[string]bool{"postgres": true, "template0": true, "template1": true}

// Restore unpacks a dump and loads it into the target database. It never touches the source
// database unless Replace is set, and then only with an explicit confirmation.
func (s *Service) Restore(ctx context.Context, request RestoreRequest) (RestoreResult, error) {
	connConfig, err := s.connection(ctx)
	if err != nil {
		return RestoreResult{}, err
	}
	entry, ok := s.catalog.get(request.ID)
	if !ok {
		return RestoreResult{}, fmt.Errorf("unknown backup %q", request.ID)
	}
	if entry.Status != "ok" {
		return RestoreResult{}, fmt.Errorf("backup %q did not complete", request.ID)
	}
	path, err := fetch(entry)
	if err != nil {
		return RestoreResult{}, err
	}
	if entry.SHA256 != "" {
		if sum, err := fileSHA256(path); err != nil || sum != entry.SHA256 {
			return RestoreResult{}, fmt.Errorf("backup %q failed its checksum", request.ID)
		}
	}
	sqlPath, cleanup, err := unzip(path)
	if err != nil {
		return RestoreResult{}, err
	}
	defer cleanup()

	target := request.Target
	if target == "" {
		target = fmt.Sprintf("%s_restore_%s", entry.Database, time.Now().UTC().Format("20060102150405"))
	}
	if err := validDatabaseName(target); err != nil {
		return RestoreResult{}, err
	}
	if request.Replace {
		if request.Confirm != target {
			return RestoreResult{}, ErrRestoreConfirm
		}
		if err := dropDatabase(ctx, connConfig, target); err != nil {
			return RestoreResult{}, err
		}
	}
	if err := createDatabase(ctx, connConfig, target); err != nil {
		return RestoreResult{}, err
	}
	if err := runRestore(ctx, connConfig, sqlPath, target); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{Target: target, Created: !request.Replace, Bytes: entry.Bytes}, nil
}

// DropDatabase removes one database through the maintenance connection. It refuses the reserved
// databases and requires the caller to repeat the name as confirmation.
func (s *Service) DropDatabase(ctx context.Context, database, confirm string) error {
	if database == "" {
		return errors.New("database name is required")
	}
	if database != confirm {
		return ErrDropConfirm
	}
	connConfig, err := s.connection(ctx)
	if err != nil {
		return err
	}
	return dropDatabase(ctx, connConfig, database)
}

// fetch returns the dump's path on disk, or an error when the file is gone.
func fetch(entry Backup) (string, error) {
	if entry.LocalPath == "" {
		return "", fmt.Errorf("backup %q has no local file", entry.ID)
	}
	if _, err := os.Stat(entry.LocalPath); err != nil {
		return "", fmt.Errorf("backup %q is missing from %s", entry.ID, entry.LocalPath)
	}
	return entry.LocalPath, nil
}

// unzip extracts the single SQL entry of a dump archive to a temp file. It is also the cheap
// integrity check: a truncated archive fails to open or to read before any target is touched.
func unzip(path string) (string, func(), error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return "", nil, fmt.Errorf("backup is not a readable archive: %w", err)
	}
	defer reader.Close()
	if len(reader.File) == 0 {
		return "", nil, errors.New("backup archive is empty")
	}
	temp, err := os.CreateTemp("", "dboss-restore-*.sql")
	if err != nil {
		return "", nil, err
	}
	sqlPath := temp.Name()
	cleanup := func() { _ = os.Remove(sqlPath) }
	source, err := reader.File[0].Open()
	if err != nil {
		_ = temp.Close()
		cleanup()
		return "", nil, err
	}
	_, copyErr := io.Copy(temp, source)
	_ = source.Close()
	if closeErr := temp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		cleanup()
		return "", nil, copyErr
	}
	return sqlPath, cleanup, nil
}

// dropDatabase is the only path to DROP DATABASE, so the reserved databases are refused here
// whichever action asked.
func dropDatabase(ctx context.Context, connConfig *pgx.ConnConfig, target string) error {
	if reservedDatabases[target] {
		return fmt.Errorf("%s is a reserved database and cannot be dropped", target)
	}
	if err := maintenanceExec(ctx, connConfig, "DROP DATABASE IF EXISTS "+pgx.Identifier{target}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop database %s: %w", target, err)
	}
	return nil
}

func createDatabase(ctx context.Context, connConfig *pgx.ConnConfig, target string) error {
	if err := maintenanceExec(ctx, connConfig, "CREATE DATABASE "+pgx.Identifier{target}.Sanitize()); err != nil {
		return fmt.Errorf("create database %s: %w", target, err)
	}
	return nil
}

// maintenanceExec runs one statement on the first of postgres or template1 that accepts a
// connection, since a database cannot be created or dropped from inside itself.
func maintenanceExec(ctx context.Context, connConfig *pgx.ConnConfig, statement string) error {
	var lastErr error
	for _, maintenance := range []string{"postgres", "template1"} {
		conn, err := pgx.ConnectConfig(ctx, forDatabase(connConfig, maintenance))
		if err != nil {
			lastErr = err
			continue
		}
		_, err = conn.Exec(ctx, statement)
		_ = conn.Close(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// forDatabase is connConfig pointed at another database on the same server.
func forDatabase(connConfig *pgx.ConnConfig, database string) *pgx.ConnConfig {
	clone := connConfig.Copy()
	clone.Database = database
	return clone
}

func runRestore(ctx context.Context, connConfig *pgx.ConnConfig, path, target string) error {
	return runClient(ctx, connConfig, "psql", "--no-psqlrc", "-v", "ON_ERROR_STOP=1", "--dbname="+databaseConnString(connConfig, target), "--file="+path)
}

// runClient runs pg_dump or psql against the server. The password travels in the environment,
// never argv, and a failure reports the tool's first output line.
func runClient(ctx context.Context, connConfig *pgx.ConnConfig, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = processEnv(connConfig)
	output, err := command.CombinedOutput()
	if err != nil {
		message := firstLine(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("%s: %s", name, message)
	}
	return nil
}
