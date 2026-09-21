package pg

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"dboss/internal/config"
	"dboss/internal/logx"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	s.mu.RLock()
	connConfig := s.connConfig
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
	if request.Replace {
		if request.Confirm != target {
			return RestoreResult{}, ErrRestoreConfirm
		}
		if err := s.dropDatabase(ctx, connConfig, target); err != nil {
			return RestoreResult{}, err
		}
	}
	if err := s.createDatabase(ctx, connConfig, target, ""); err != nil {
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
	if reservedDatabases[database] {
		return fmt.Errorf("%s is a reserved database and cannot be dropped", database)
	}
	s.mu.RLock()
	connConfig := s.connConfig
	s.mu.RUnlock()
	if connConfig == nil {
		return errors.New("no reachable PostgreSQL server")
	}
	return s.dropDatabase(ctx, connConfig, database)
}

// AppDatabases resolves one app's pg_db block into the environment values its processes are
// spawned with, creating a database that does not exist yet. Entries are keyed by the variable
// name they are exported as; a value is either a database name on the host's own server or a
// full connection URL, which is created on the server it names and passed through unchanged.
// An unreachable host server is an error whatever the entries look like, so the supervisor
// refuses the start instead of running the app without its connection URLs.
func (s *Service) AppDatabases(ctx context.Context, app string, databases map[string]config.PgDBSpec) (map[string]string, error) {
	if len(databases) == 0 {
		return nil, nil
	}
	s.mu.RLock()
	connConfig := s.connConfig
	s.mu.RUnlock()
	if connConfig == nil {
		return nil, errors.New("no reachable PostgreSQL server")
	}
	names := make([]string, 0, len(databases))
	for name := range databases {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make(map[string]string, len(databases))
	for _, name := range names {
		entry := databases[name]
		// A full URL owns its server, so it is created there and handed to the app exactly as
		// written; only a bare database name is resolved against the host's own connection.
		if config.PgDBIsURL(entry.Database) {
			target, err := pgx.ParseConfig(entry.Database)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			database, err := config.PgDBDatabase(entry.Database)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if err := s.ensureDatabase(ctx, target, app, database, entry.Template); err != nil {
				return nil, err
			}
			env[name] = entry.Database
			continue
		}
		if err := s.ensureDatabase(ctx, connConfig, app, entry.Database, entry.Template); err != nil {
			return nil, err
		}
		env[name] = databaseURL(connConfig, entry.Database)
	}
	return env, nil
}

// ensureDatabase creates database when the server does not have it yet. The lookup runs on the
// maintenance connection, so it costs one indexed read on an app that is already set up.
func (s *Service) ensureDatabase(ctx context.Context, connConfig *pgx.ConnConfig, app, database, template string) error {
	exists, err := s.databaseExists(ctx, connConfig, database)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	// Checking the template costs a query on the create path only, and buys an error that names
	// dboss's own config instead of a bare SQLSTATE from the server.
	if template != "" {
		ok, err := s.databaseExists(ctx, connConfig, template)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("create database %s for app %s: template database %s does not exist on that server", database, app, template)
		}
	}
	if err := s.createDatabase(ctx, connConfig, database, template); err != nil {
		return err
	}
	if template != "" {
		logx.Infof("postgres: created database %s from template %s for app %s", database, template, app)
		return nil
	}
	logx.Infof("postgres: created database %s for app %s", database, app)
	return nil
}

func (s *Service) databaseExists(ctx context.Context, connConfig *pgx.ConnConfig, database string) (bool, error) {
	var lastErr error
	for _, maintenance := range []string{"postgres", "template1"} {
		conn, err := pgx.ConnectConfig(ctx, mustConfig(connConfig, maintenance))
		if err != nil {
			lastErr = err
			continue
		}
		var found bool
		err = conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", database).Scan(&found)
		_ = conn.Close(ctx)
		if err == nil {
			return found, nil
		}
		lastErr = err
	}
	return false, fmt.Errorf("look up database %s: %w", database, lastErr)
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

func (s *Service) dropDatabase(ctx context.Context, connConfig *pgx.ConnConfig, target string) error {
	var lastErr error
	for _, maintenance := range []string{"postgres", "template1"} {
		conn, err := pgx.ConnectConfig(ctx, mustConfig(connConfig, maintenance))
		if err != nil {
			lastErr = err
			continue
		}
		_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{target}.Sanitize()+" WITH (FORCE)")
		_ = conn.Close(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("drop database %s: %w", target, lastErr)
}

// createDatabase connects to the maintenance database and creates the target. It uses the first
// of postgres or template1 that accepts a connection.
func (s *Service) createDatabase(ctx context.Context, connConfig *pgx.ConnConfig, target, template string) error {
	var lastErr error
	for _, maintenance := range []string{"postgres", "template1"} {
		// Any session on the template blocks the copy, including ours: never hold the template
		// open as the maintenance database.
		if maintenance == template {
			continue
		}
		conn, err := pgx.ConnectConfig(ctx, mustConfig(connConfig, maintenance))
		if err != nil {
			lastErr = err
			continue
		}
		_, err = conn.Exec(ctx, createStatement(target, template))
		_ = conn.Close(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	if template != "" {
		return createTemplateError(target, template, lastErr)
	}
	return fmt.Errorf("create database %s: %w", target, lastErr)
}

// createStatement builds the create. Each identifier is sanitised on its own: pgx.Identifier with
// two parts renders a qualified "a"."b", which is not what CREATE DATABASE ... TEMPLATE wants.
func createStatement(target, template string) string {
	statement := "CREATE DATABASE " + pgx.Identifier{target}.Sanitize()
	if template != "" {
		statement += " TEMPLATE " + pgx.Identifier{template}.Sanitize()
	}
	return statement
}

// createTemplateError turns the failures a template create really hits into an answer an operator
// can act on. 55006 is the one everybody meets, and the server names no way out of it.
func createTemplateError(target, template string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "55006":
			return fmt.Errorf("create database %s from template %s: another session is connected to %s, and PostgreSQL only copies a template nobody is using. Close those sessions, or run SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s', then start the app again", target, template, template, template)
		case "3D000":
			return fmt.Errorf("create database %s: template database %s does not exist on that server", target, template)
		case "42501":
			return fmt.Errorf("create database %s from template %s: this role may not copy %s; it has to own the template or be a superuser", target, template, template)
		}
	}
	return fmt.Errorf("create database %s from template %s: %w", target, template, err)
}

func mustConfig(connConfig *pgx.ConnConfig, database string) *pgx.ConnConfig {
	clone := connConfig.Copy()
	clone.Database = database
	return clone
}

func runRestore(ctx context.Context, connConfig *pgx.ConnConfig, path, target string) error {
	command := exec.CommandContext(ctx, "psql", "--no-psqlrc", "-v", "ON_ERROR_STOP=1", "--dbname="+databaseConnString(connConfig, target), "--file="+path)
	command.Env = processEnv(connConfig)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", filepath.Base(command.Path), firstLine(string(output)))
	}
	return nil
}
