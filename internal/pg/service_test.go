package pg

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"dboss/internal/config"

	"github.com/jackc/pgx/v5"
)

func connConfig(t *testing.T, dsn string) *pgx.ConnConfig {
	t.Helper()
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return connConfig
}

func TestDumpNameIsBackupZip(t *testing.T) {
	at := time.Date(2026, 9, 19, 14, 30, 5, 0, time.UTC)
	if got := dumpName("app", at); got != "BACKUP_2026-09-19T14-30-05Z.zip" {
		t.Fatalf("dumpName = %q", got)
	}
}

func TestNextRunUsesUTCTime(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if got := nextRun("04:00", now); !got.Equal(time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("nextRun(past) = %s, want tomorrow 04:00 UTC", got)
	}
	if got := nextRun("18:00", now); !got.Equal(time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("nextRun(future) = %s, want today 18:00 UTC", got)
	}
	if got := nextRun("", now); !got.IsZero() {
		t.Fatalf("empty at should disable the schedule, got %s", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:           "0B",
		1023:        "1023B",
		1024:        "1.0K",
		1536:        "1.5K",
		1024 * 1024: "1.0M",
	}
	for size, want := range cases {
		if got := humanBytes(size); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", size, got, want)
		}
	}
}

func TestConnStringsStripThePassword(t *testing.T) {
	connConfig := connConfig(t, "postgres://app:secret@127.0.0.1:5432/app?sslmode=disable")
	if got := serverConnString(connConfig); strings.Contains(got, "secret") {
		t.Fatalf("serverConnString leaked the password: %q", got)
	}
	if got := databaseConnString(connConfig, "reports"); !strings.Contains(got, "reports") || strings.Contains(got, "secret") {
		t.Fatalf("databaseConnString = %q", got)
	}
	if got := connConfig.ConnString(); !strings.Contains(got, "secret") {
		t.Fatalf("databaseConnString must not mutate the original: %q", got)
	}
}

func TestConnStringWithoutPasswordHandlesKeywordValue(t *testing.T) {
	connConfig := connConfig(t, "host=/var/run/postgresql user=app password=secret application_name='my app'")
	got := databaseConnString(connConfig, "reports")
	if strings.Contains(got, "secret") {
		t.Fatalf("keyword/value conn string leaked the password: %q", got)
	}
	for _, want := range []string{"dbname=reports", "user=app", "host=/var/run/postgresql", "application_name='my app'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("conn string missing %q: %q", want, got)
		}
	}
}

func TestDescribeNamesSocketAndHost(t *testing.T) {
	if got := describe(nil); got != "not configured" {
		t.Fatalf("describe(nil) = %q", got)
	}
	socket := connConfig(t, "postgres://app@127.0.0.1:5432/app?sslmode=disable")
	socket.Host = "/var/run/postgresql"
	if got := describe(socket); !strings.HasPrefix(got, "unix socket") || !strings.Contains(got, "user app") {
		t.Fatalf("describe(socket) = %q", got)
	}
	host := connConfig(t, "postgres://app@db.internal:5432/app?sslmode=disable")
	if got := describe(host); got != "db.internal:5432 user app" {
		t.Fatalf("describe(host) = %q", got)
	}
}

func TestProcessEnvCarriesTheConnection(t *testing.T) {
	env := processEnv(connConfig(t, "postgres://app:secret@db.internal:5432/app?sslmode=disable"))
	joined := strings.Join(env, "\n")
	for _, want := range []string{"PGHOST=db.internal", "PGPORT=5432", "PGUSER=app", "PGPASSWORD=secret", "PGSSLMODE=disable"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("processEnv missing %q:\n%s", want, joined)
		}
	}
}

func TestCatalogRecordsAndForgets(t *testing.T) {
	catalog := newCatalog(t.TempDir())
	first := Backup{ID: "a", Database: "app", Time: "2026-09-19T10:00:00Z"}
	second := Backup{ID: "b", Database: "app", Time: "2026-09-19T11:00:00Z"}
	if err := catalog.record(first); err != nil {
		t.Fatal(err)
	}
	if err := catalog.record(second); err != nil {
		t.Fatal(err)
	}
	if got := catalog.list(); len(got) != 2 || got[0].ID != "b" {
		t.Fatalf("list = %v, want newest first", got)
	}
	if entry, ok := catalog.get("a"); !ok || entry.Database != "app" {
		t.Fatalf("get(a) = %v, %v", entry, ok)
	}
	if err := catalog.forget(map[string]bool{"a": true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.get("a"); ok {
		t.Fatal("forget did not drop a")
	}
	if got := catalog.list(); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("list after forget = %v", got)
	}
}

func TestDatabaseURLKeepsCredentialsAndSwapsDatabase(t *testing.T) {
	cases := []struct {
		name     string
		dsn      string
		contains []string
		absent   []string
	}{
		{
			name:     "url with password",
			dsn:      "postgres://app:secret@db.internal:5433/other?sslmode=disable",
			contains: []string{"postgres://app:secret@db.internal:5433/reports", "sslmode=disable"},
			absent:   []string{"/other"},
		},
		{
			name:     "keyword form over a unix socket",
			dsn:      "host=/var/run/postgresql user=app application_name='my app'",
			contains: []string{"postgres://app@/reports", "host=%2Fvar%2Frun%2Fpostgresql", "application_name=my+app"},
		},
		{
			// Characters that mean something in a URL have to survive the encode/decode round
			// trip, because the app's driver parses this string.
			name:     "password full of url punctuation",
			dsn:      "host=127.0.0.1 port=5432 user=app password='p@ss:w/rd?#&x'",
			contains: []string{"@127.0.0.1:5432/reports"},
			absent:   []string{"p@ss:w/rd"},
		},
		{
			name:     "tcp without a password",
			dsn:      "host=127.0.0.1 port=5432 user=app",
			contains: []string{"postgres://app@127.0.0.1:5432/reports"},
			absent:   []string{"@:"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := databaseURL(connConfig(t, test.dsn), "reports")
			for _, want := range test.contains {
				if !strings.Contains(got, want) {
					t.Errorf("databaseURL = %q, want it to contain %q", got, want)
				}
			}
			for _, unwanted := range test.absent {
				if strings.Contains(got, unwanted) {
					t.Errorf("databaseURL = %q, want it to drop %q", got, unwanted)
				}
			}
			if _, err := pgx.ParseConfig(got); err != nil {
				t.Fatalf("databaseURL produced an unparseable DSN %q: %v", got, err)
			}
			// The URL is read by whatever driver the app uses, so every field has to survive a
			// plain URL parse too, not just pgx. Ruby's Sequel, for one, parses it itself.
			parsed, err := url.Parse(got)
			if err != nil {
				t.Fatalf("databaseURL is not a valid URL %q: %v", got, err)
			}
			if parsed.Path != "/reports" {
				t.Errorf("path = %q, want /reports", parsed.Path)
			}
			source := connConfig(t, test.dsn)
			if parsed.User.Username() != source.User {
				t.Errorf("user = %q, want %q", parsed.User.Username(), source.User)
			}
			password, _ := parsed.User.Password()
			if password != source.Password {
				t.Errorf("password = %q, want %q", password, source.Password)
			}
		})
	}
}

// TestAppDatabasesAgainstLiveServer exercises the real create-and-export path. It skips unless a
// PostgreSQL server is reachable, so it is a no-op on a box without one.
func TestAppDatabasesAgainstLiveServer(t *testing.T) {
	service := New(config.Config{StateDir: t.TempDir(), Postgres: config.Postgres{Enabled: true}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	service.detect(ctx)
	if service.connConfig == nil {
		t.Skip("no reachable PostgreSQL server")
	}
	database := fmt.Sprintf("dboss_pgdb_test_%d", time.Now().UnixNano())
	defer func() { _ = service.dropDatabase(context.Background(), service.connConfig, database) }()

	env, err := service.AppDatabases(ctx, "demo", map[string]string{"DB_MAIN": database})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env["DB_MAIN"], database) {
		t.Fatalf("DB_MAIN = %q, want the database in it", env["DB_MAIN"])
	}
	// The URL must actually connect, and land in the database it names.
	conn, err := pgx.Connect(ctx, env["DB_MAIN"])
	if err != nil {
		t.Fatalf("connect with %q: %v", env["DB_MAIN"], err)
	}
	var current string
	err = conn.QueryRow(ctx, "SELECT current_database()").Scan(&current)
	_ = conn.Close(ctx)
	if err != nil || current != database {
		t.Fatalf("current_database = %q (%v), want %q", current, err, database)
	}
	// A second call is a no-op on an existing database and returns the same URL.
	again, err := service.AppDatabases(ctx, "demo", map[string]string{"DB_MAIN": database})
	if err != nil || again["DB_MAIN"] != env["DB_MAIN"] {
		t.Fatalf("second call = %v, %v", again, err)
	}
}

// TestAppDatabasesCreatesFromAFullURL points a pg_db URL entry at the local server, which is the
// only server a test can create on. It skips unless one is reachable.
func TestAppDatabasesCreatesFromAFullURL(t *testing.T) {
	service := New(config.Config{StateDir: t.TempDir(), Postgres: config.Postgres{Enabled: true}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	service.detect(ctx)
	if service.connConfig == nil {
		t.Skip("no reachable PostgreSQL server")
	}
	database := fmt.Sprintf("dboss_pgdb_url_test_%d", time.Now().UnixNano())
	target := databaseURL(service.connConfig, database)
	defer func() { _ = service.dropDatabase(context.Background(), service.connConfig, database) }()

	env, err := service.AppDatabases(ctx, "demo", map[string]string{"DB_REPORT": target})
	if err != nil {
		t.Fatal(err)
	}
	// The URL is handed back exactly as written, not rebuilt from the host connection.
	if env["DB_REPORT"] != target {
		t.Fatalf("DB_REPORT = %q, want the URL unchanged %q", env["DB_REPORT"], target)
	}
	conn, err := pgx.Connect(ctx, env["DB_REPORT"])
	if err != nil {
		t.Fatalf("the URL entry did not create a connectable database: %v", err)
	}
	var current string
	err = conn.QueryRow(ctx, "SELECT current_database()").Scan(&current)
	_ = conn.Close(ctx)
	if err != nil || current != database {
		t.Fatalf("current_database = %q (%v), want %q", current, err, database)
	}
}
