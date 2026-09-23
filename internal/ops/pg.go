package ops

import (
	"context"
	"errors"
	"io"

	"dboss/internal/config"
	"dboss/internal/pg"
)

// PGSnapshot returns the PostgreSQL inspection, refreshing it first when asked.
func (s *Service) PGSnapshot(refresh bool) (pg.Snapshot, error) {
	postgres, err := s.postgres()
	if err != nil {
		return pg.Snapshot{}, err
	}
	if refresh {
		return postgres.Refresh(context.Background()), nil
	}
	return postgres.Snapshot(), nil
}

// errNoPostgres answers every PG action while the feature is off.
var errNoPostgres = errors.New("postgres is not enabled")

// postgres is the PG service when the operator turned it on.
func (s *Service) postgres() (PG, error) {
	if s.pg == nil || !s.pg.Enabled() {
		return nil, errNoPostgres
	}
	return s.pg, nil
}

// PGAvailable reports whether the last inspection reached a server, for the console's tab gate.
func (s *Service) PGAvailable() bool { return s.pg != nil && s.pg.Enabled() && s.pg.Available() }

// Backups lists the recorded dumps.
func (s *Service) Backups() []pg.Backup {
	if s.pg == nil {
		return nil
	}
	return s.pg.Backups()
}

// runBackup dumps one database, or every selected database when name is empty. An explicit run is
// a manual backup, so it is never rotated away.
func (s *Service) runBackup(database string) ([]pg.Backup, error) {
	postgres, err := s.postgres()
	if err != nil {
		return nil, err
	}
	if database == "" {
		return nil, postgres.BackupAll(context.Background())
	}
	entry, err := postgres.BackupDatabase(context.Background(), database, true)
	return []pg.Backup{entry}, err
}

// restore loads a recorded dump into a database.
func (s *Service) restore(request pg.RestoreRequest) (pg.RestoreResult, error) {
	postgres, err := s.postgres()
	if err != nil {
		return pg.RestoreResult{}, err
	}
	return postgres.Restore(context.Background(), request)
}

// dropDatabase removes a database. The operator must repeat the name as confirmation.
func (s *Service) dropDatabase(database, confirm string) error {
	postgres, err := s.postgres()
	if err != nil {
		return err
	}
	return postgres.DropDatabase(context.Background(), database, confirm)
}

// runQuery executes SQL against one database and returns its last result set. It is a mutating
// action by nature, so it goes through Do and is audited with the statement.
func (s *Service) runQuery(database, sql string) (pg.QueryResult, error) {
	postgres, err := s.postgres()
	if err != nil {
		return pg.QueryResult{}, err
	}
	return postgres.Query(context.Background(), database, sql)
}

// BackupFile returns one recorded dump and the archive's path on disk, for the console download.
func (s *Service) BackupFile(id string) (pg.Backup, string, error) {
	postgres, err := s.postgres()
	if err != nil {
		return pg.Backup{}, "", err
	}
	return postgres.BackupFile(id)
}

// ImportBackup stores an uploaded archive for a database as a manual dump, so it can be restored
// like any other recorded backup.
func (s *Service) ImportBackup(database string, source io.Reader) (pg.Backup, error) {
	postgres, err := s.postgres()
	if err != nil {
		return pg.Backup{}, err
	}
	return postgres.ImportBackup(database, source)
}

// deleteBackup removes one recorded dump from disk and the catalog.
func (s *Service) deleteBackup(id string) error {
	postgres, err := s.postgres()
	if err != nil {
		return err
	}
	return postgres.DeleteBackup(id)
}

// PGBackupConfig returns the effective PostgreSQL backup policy.
func (s *Service) PGBackupConfig() config.PostgresBackups {
	if s.pg == nil {
		return config.PostgresBackups{}
	}
	return s.pg.BackupConfig()
}
