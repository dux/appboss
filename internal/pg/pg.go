// Package pg inspects the host's PostgreSQL server and backs up the databases the operator
// selects. Inspection is read-only; backups shell out to pg_dump and copy the result locally
// and, when configured, to S3. The daemon keeps one Service and registers it as a module.
package pg

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"dboss/internal/config"

	"github.com/jackc/pgx/v5"
)

// connectTimeout bounds a detection or inspection connection attempt.
const connectTimeout = 5 * time.Second

// candidateDSNs returns the connection strings detection tries in order. An explicit DSN is used
// as is; otherwise the local unix sockets are tried before loopback TCP. The libpq PG* environment
// is always merged by pgx, so PGUSER/PGPASSWORD/PGDATABASE still apply.
//
// A daemon started with sudo runs as root, whose matching Postgres role does not exist, so
// detection impersonates the invoking user (SUDO_USER) when one is available. That is what makes
// the local macOS demo reach Postgres.app; a real service user has its own role and is unaffected.
func candidateDSNs(dsn string) []string {
	if strings.TrimSpace(dsn) != "" {
		return []string{strings.TrimSpace(dsn)}
	}
	user := ""
	if os.Geteuid() == 0 {
		user = strings.TrimSpace(os.Getenv("SUDO_USER"))
	}
	account := func(host string) string {
		if user == "" {
			return host
		}
		return host + " user=" + user
	}
	return []string{account("host=/var/run/postgresql"), account("host=/tmp"), account("host=127.0.0.1 port=5432")}
}

// resolve connects to the first reachable candidate and returns its parsed config. A nil result
// with nil error means no candidate accepted a connection, which is the normal "no local server"
// case.
func resolve(ctx context.Context, dsn string) (*pgx.ConnConfig, error) {
	var lastErr error
	for _, candidate := range candidateDSNs(dsn) {
		connConfig, err := pgx.ParseConfig(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		connConfig.ConnectTimeout = connectTimeout
		connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		conn, err := pgx.ConnectConfig(connectCtx, connConfig)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		_ = conn.Close(ctx)
		return connConfig, nil
	}
	return nil, lastErr
}

// connect opens a fresh connection with the configured timeout. Inspection uses a short-lived
// connection per refresh, so a server that goes away never leaves a stale pool behind.
func connect(ctx context.Context, connConfig *pgx.ConnConfig) (*pgx.Conn, error) {
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	return pgx.ConnectConfig(connectCtx, connConfig)
}

// processEnv is the libpq environment for a child process: the resolved host, port, user and
// password so pg_dump and pg_dumpall connect exactly as the inspector does. The password is
// passed through the environment, never argv, so it stays out of the process list.
func processEnv(connConfig *pgx.ConnConfig) []string {
	env := os.Environ()
	add := func(name, value string) {
		if value != "" {
			env = append(env, name+"="+value)
		}
	}
	add("PGHOST", connConfig.Host)
	add("PGPORT", strconv.Itoa(int(connConfig.Port)))
	add("PGUSER", connConfig.User)
	add("PGPASSWORD", connConfig.Password)
	if connConfig.TLSConfig == nil {
		add("PGSSLMODE", "disable")
	}
	return env
}

// databaseConnString is the connection string for one database. It keeps every option of the
// resolved DSN but never the password, which travels through PGPASSWORD; pgx's ConnString returns
// the original string unchanged, so the password is removed here instead.
func databaseConnString(connConfig *pgx.ConnConfig, database string) string {
	return connStringWithoutPassword(connConfig, database)
}

func serverConnString(connConfig *pgx.ConnConfig) string {
	return connStringWithoutPassword(connConfig, "")
}

// connStringWithoutPassword rebuilds a libpq connection string with the password removed and,
// when database is set, its dbname replaced. It accepts both postgres:// URLs and keyword/value
// strings, and is the only form ever placed in a child process's argv.
func connStringWithoutPassword(connConfig *pgx.ConnConfig, database string) string {
	original := connConfig.ConnString()
	if strings.TrimSpace(original) == "" {
		settings := map[string]string{"host": connConfig.Host, "port": strconv.Itoa(int(connConfig.Port)), "user": connConfig.User}
		if database != "" {
			settings["dbname"] = database
		}
		return formatConnSettings(settings)
	}
	if parsed, err := url.Parse(original); err == nil && (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") {
		if parsed.User != nil {
			parsed.User = url.User(parsed.User.Username())
		}
		if database != "" {
			parsed.Path = "/" + database
		}
		return parsed.String()
	}
	settings := parseConnSettings(original)
	delete(settings, "password")
	if database != "" {
		settings["dbname"] = database
	}
	return formatConnSettings(settings)
}

// parseConnSettings tokenizes the libpq keyword/value form, honoring the single-quote and
// backslash escaping libpq accepts.
func parseConnSettings(connString string) map[string]string {
	settings := map[string]string{}
	skipSpace := func(i int) int {
		for i < len(connString) && (connString[i] == ' ' || connString[i] == '\t') {
			i++
		}
		return i
	}
	for i := 0; i < len(connString); {
		i = skipSpace(i)
		start := i
		for i < len(connString) && connString[i] != '=' && connString[i] != ' ' && connString[i] != '\t' {
			i++
		}
		key := connString[start:i]
		i = skipSpace(i)
		if key == "" {
			i++
			continue
		}
		if i >= len(connString) || connString[i] != '=' {
			settings[key] = key
			continue
		}
		i++
		i = skipSpace(i)
		var value strings.Builder
		if i < len(connString) && connString[i] == '\'' {
			i++
			for i < len(connString) {
				if connString[i] == '\\' && i+1 < len(connString) {
					value.WriteByte(connString[i+1])
					i += 2
					continue
				}
				if connString[i] == '\'' {
					i++
					break
				}
				value.WriteByte(connString[i])
				i++
			}
		} else {
			start = i
			for i < len(connString) && connString[i] != ' ' && connString[i] != '\t' {
				i++
			}
			value.WriteString(connString[start:i])
		}
		settings[key] = value.String()
	}
	return settings
}

// formatConnSettings renders settings as a libpq keyword/value string, quoting when needed.
func formatConnSettings(settings map[string]string) string {
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := settings[key]
		if value == "" {
			continue
		}
		if strings.ContainsAny(value, " \t'\\") {
			value = "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'").Replace(value) + "'"
		}
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, " ")
}

func describe(connConfig *pgx.ConnConfig) string {
	if connConfig == nil {
		return "not configured"
	}
	if strings.HasPrefix(connConfig.Host, "/") {
		return fmt.Sprintf("unix socket %s port %d user %s", connConfig.Host, connConfig.Port, connConfig.User)
	}
	return fmt.Sprintf("%s:%d user %s", connConfig.Host, connConfig.Port, connConfig.User)
}

// options is the pair the service is rebuilt from on every config reload.
type options struct {
	postgres config.Postgres
	s3       config.S3
	stateDir string
	logDir   string
}

func optionsFrom(cfg config.Config) options {
	return options{postgres: cfg.Postgres, s3: cfg.S3, stateDir: cfg.StateDir, logDir: cfg.LogDir}
}
