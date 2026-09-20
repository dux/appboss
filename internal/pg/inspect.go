package pg

import (
	"context"
	"strings"
	"time"

	"dboss/internal/config"

	"github.com/jackc/pgx/v5"
)

// Snapshot is one read-only inspection of the server. Available is false when no server could be
// reached, in which case Error explains why and every other field is zero.
type Snapshot struct {
	CollectedAt time.Time    `json:"collected_at"`
	Available   bool         `json:"available"`
	Error       string       `json:"error,omitempty"`
	Server      Server       `json:"server"`
	Databases   []Database   `json:"databases"`
	Activity    Activity     `json:"activity"`
	Tablespaces []Tablespace `json:"tablespaces,omitempty"`
}

// Server is the cluster-level state operators look at first.
type Server struct {
	Description        string    `json:"description,omitempty"`
	Version            string    `json:"version"`
	VersionNum         int       `json:"version_num"`
	StartedAt          time.Time `json:"started_at,omitempty"`
	UptimeSeconds      int64     `json:"uptime_seconds"`
	Host               string    `json:"host"`
	Port               int       `json:"port"`
	DataDir            string    `json:"data_dir,omitempty"`
	MaxConnections     int       `json:"max_connections"`
	CurrentConnections int       `json:"current_connections"`
	CacheHitRatio      float64   `json:"cache_hit_ratio"`
	WALLSN             string    `json:"wal_lsn,omitempty"`
	InRecovery         bool      `json:"in_recovery"`
	Replicas           []Replica `json:"replicas,omitempty"`
}

// Replica is one streaming replica as seen from the primary.
type Replica struct {
	ClientAddr string `json:"client_addr"`
	State      string `json:"state"`
	SyncState  string `json:"sync_state"`
	LagBytes   int64  `json:"lag_bytes"`
}

// Database is one database with its size, connections and the backup selection attached.
type Database struct {
	Name           string `json:"name"`
	Owner          string `json:"owner"`
	SizeBytes      int64  `json:"size_bytes"`
	Encoding       string `json:"encoding"`
	Collate        string `json:"collate"`
	Connections    int    `json:"connections"`
	XactCommit     int64  `json:"xact_commit"`
	XactRollback   int64  `json:"xact_rollback"`
	Deadlocks      int64  `json:"deadlocks"`
	TempBytes      int64  `json:"temp_bytes"`
	BackupSelected bool   `json:"backup_selected"`
	BackupRotation string `json:"backup_rotation,omitempty"`
}

// Activity summarizes what the server is doing right now.
type Activity struct {
	Active            int     `json:"active"`
	Idle              int     `json:"idle"`
	IdleInTransaction int     `json:"idle_in_transaction"`
	Waiting           int     `json:"waiting"`
	Total             int     `json:"total"`
	Blocked           int     `json:"blocked"`
	LongestSeconds    float64 `json:"longest_seconds"`
	LongestQuery      string  `json:"longest_query,omitempty"`
}

// Tablespace is one tablespace with its location and size.
type Tablespace struct {
	Name      string `json:"name"`
	Location  string `json:"location"`
	SizeBytes int64  `json:"size_bytes"`
}

const serverQuery = `
SELECT version(),
       current_setting('server_version_num')::int,
       pg_postmaster_start_time(),
       GREATEST(0, extract(epoch FROM now() - pg_postmaster_start_time()))::bigint,
       current_setting('max_connections')::int,
       COALESCE(current_setting('data_directory', true), ''),
       COALESCE(pg_is_in_recovery(), false),
       COALESCE(CASE WHEN pg_is_in_recovery() THEN pg_last_wal_receive_lsn() ELSE pg_current_wal_lsn() END::text, '')
`

const connectionCountQuery = `
SELECT count(*)::int
FROM pg_stat_activity
WHERE backend_type = 'client backend'
`

const cacheHitQuery = `
SELECT COALESCE(sum(blks_hit)::float / NULLIF(sum(blks_hit) + sum(blks_read), 0), 0)::float
FROM pg_stat_database
`

const databaseQuery = `
SELECT d.datname,
       COALESCE(pg_get_userbyid(d.datdba), ''),
       pg_database_size(d.datname),
       COALESCE(pg_encoding_to_char(d.encoding), ''),
       COALESCE(d.datcollate, ''),
       COALESCE(s.numbackends, 0)::int,
       COALESCE(s.xact_commit, 0),
       COALESCE(s.xact_rollback, 0),
       COALESCE(s.deadlocks, 0),
       COALESCE(s.temp_bytes, 0)
FROM pg_database d
LEFT JOIN pg_stat_database s ON s.datid = d.oid
WHERE NOT d.datistemplate
ORDER BY d.datname
`

const activityQuery = `
SELECT count(*) FILTER (WHERE state = 'active')::int,
       count(*) FILTER (WHERE state = 'idle')::int,
       count(*) FILTER (WHERE state = 'idle in transaction')::int,
       count(*) FILTER (WHERE state = 'active' AND wait_event_type IS NOT NULL)::int,
       count(*)::int
FROM pg_stat_activity
WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()
`

const longestQuery = `
SELECT COALESCE(max(extract(epoch FROM now() - query_start)), 0)::float,
       COALESCE((array_agg(left(query, 300) ORDER BY query_start ASC))[1], '')
FROM pg_stat_activity
WHERE backend_type = 'client backend' AND state = 'active' AND pid <> pg_backend_pid() AND query_start IS NOT NULL
`

const blockedQuery = `
SELECT count(DISTINCT pid)::int
FROM pg_stat_activity
WHERE wait_event_type = 'Lock'
`

const replicaQuery = `
SELECT COALESCE(client_addr::text, ''),
       COALESCE(state, ''),
       COALESCE(sync_state, ''),
       COALESCE(GREATEST(0, pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)), 0)::bigint
FROM pg_stat_replication
ORDER BY client_addr
`

const tablespaceQuery = `
SELECT spcname,
       COALESCE(pg_tablespace_location(oid), ''),
       COALESCE(pg_tablespace_size(oid), 0)
FROM pg_tablespace
WHERE spcname NOT IN ('pg_default', 'pg_global')
ORDER BY spcname
`

// collect runs every inspection query against one open connection. A failure in the optional
// sections (replication, tablespaces) is tolerated, since a restricted role may not read them.
func collect(ctx context.Context, conn *pgx.Conn, connConfig *pgx.ConnConfig, backupConfig config.PostgresBackup) Snapshot {
	snapshot := Snapshot{CollectedAt: time.Now(), Server: Server{Description: describe(connConfig)}}
	if connConfig != nil {
		snapshot.Server.Host = connConfig.Host
		snapshot.Server.Port = int(connConfig.Port)
	}
	row := conn.QueryRow(ctx, serverQuery)
	if err := row.Scan(&snapshot.Server.Version, &snapshot.Server.VersionNum, &snapshot.Server.StartedAt, &snapshot.Server.UptimeSeconds, &snapshot.Server.MaxConnections, &snapshot.Server.DataDir, &snapshot.Server.InRecovery, &snapshot.Server.WALLSN); err != nil {
		snapshot.Error = err.Error()
		return snapshot
	}
	snapshot.Available = true
	snapshot.Server.Version = firstLine(snapshot.Server.Version)
	_ = conn.QueryRow(ctx, connectionCountQuery).Scan(&snapshot.Server.CurrentConnections)
	_ = conn.QueryRow(ctx, cacheHitQuery).Scan(&snapshot.Server.CacheHitRatio)
	snapshot.Databases = collectDatabases(ctx, conn, backupConfig)
	snapshot.Activity = collectActivity(ctx, conn)
	snapshot.Server.Replicas = collectReplicas(ctx, conn)
	snapshot.Tablespaces = collectTablespaces(ctx, conn)
	return snapshot
}

func collectDatabases(ctx context.Context, conn *pgx.Conn, backupConfig config.PostgresBackup) []Database {
	rows, err := conn.Query(ctx, databaseQuery)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var databases []Database
	for rows.Next() {
		var database Database
		if err := rows.Scan(&database.Name, &database.Owner, &database.SizeBytes, &database.Encoding, &database.Collate, &database.Connections, &database.XactCommit, &database.XactRollback, &database.Deadlocks, &database.TempBytes); err != nil {
			continue
		}
		_, database.BackupSelected = backupConfig.Databases[database.Name]
		database.BackupRotation = backupConfig.Rotation(database.Name)
		databases = append(databases, database)
	}
	return databases
}

func collectActivity(ctx context.Context, conn *pgx.Conn) Activity {
	var activity Activity
	if err := conn.QueryRow(ctx, activityQuery).Scan(&activity.Active, &activity.Idle, &activity.IdleInTransaction, &activity.Waiting, &activity.Total); err != nil {
		return Activity{}
	}
	_ = conn.QueryRow(ctx, longestQuery).Scan(&activity.LongestSeconds, &activity.LongestQuery)
	_ = conn.QueryRow(ctx, blockedQuery).Scan(&activity.Blocked)
	activity.LongestQuery = firstLine(activity.LongestQuery)
	return activity
}

func collectReplicas(ctx context.Context, conn *pgx.Conn) []Replica {
	rows, err := conn.Query(ctx, replicaQuery)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var replicas []Replica
	for rows.Next() {
		var replica Replica
		if err := rows.Scan(&replica.ClientAddr, &replica.State, &replica.SyncState, &replica.LagBytes); err != nil {
			continue
		}
		replicas = append(replicas, replica)
	}
	return replicas
}

func collectTablespaces(ctx context.Context, conn *pgx.Conn) []Tablespace {
	rows, err := conn.Query(ctx, tablespaceQuery)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var tablespaces []Tablespace
	for rows.Next() {
		var tablespace Tablespace
		if err := rows.Scan(&tablespace.Name, &tablespace.Location, &tablespace.SizeBytes); err != nil {
			continue
		}
		tablespaces = append(tablespaces, tablespace)
	}
	return tablespaces
}

func firstLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
