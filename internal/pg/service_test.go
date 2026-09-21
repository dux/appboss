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

	env, err := service.AppDatabases(ctx, "demo", map[string]config.PgDBSpec{"DB_MAIN": {Database: database}})
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
	again, err := service.AppDatabases(ctx, "demo", map[string]config.PgDBSpec{"DB_MAIN": {Database: database}})
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

	env, err := service.AppDatabases(ctx, "demo", map[string]config.PgDBSpec{"DB_REPORT": {Database: target}})
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

// TestAppDatabasesFromATemplateAgainstLiveServer exercises the real copy path. It skips unless a
// PostgreSQL server is reachable, so it is a no-op on a box without one.
func TestAppDatabasesFromATemplateAgainstLiveServer(t *testing.T) {
	service := New(config.Config{StateDir: t.TempDir(), Postgres: config.Postgres{Enabled: true}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	service.detect(ctx)
	if service.connConfig == nil {
		t.Skip("no reachable PostgreSQL server")
	}
	stamp := time.Now().UnixNano()
	template := fmt.Sprintf("dboss_tpl_test_%d", stamp)
	target := fmt.Sprintf("dboss_tpl_copy_%d", stamp)
	defer func() {
		_ = service.dropDatabase(context.Background(), service.connConfig, target)
		_ = service.dropDatabase(context.Background(), service.connConfig, template)
	}()

	if err := service.createDatabase(ctx, service.connConfig, template, ""); err != nil {
		t.Fatal(err)
	}
	seed, err := pgx.Connect(ctx, databaseURL(service.connConfig, template))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(ctx, "CREATE TABLE marker (note text)"); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(ctx, "INSERT INTO marker VALUES ('from the template')"); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close(ctx)

	env, err := service.AppDatabases(ctx, "demo", map[string]config.PgDBSpec{"DB_MAIN": {Database: target, Template: template}})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, env["DB_MAIN"])
	if err != nil {
		t.Fatalf("connect with %q: %v", env["DB_MAIN"], err)
	}
	var note string
	err = conn.QueryRow(ctx, "SELECT note FROM marker").Scan(&note)
	_ = conn.Close(ctx)
	if err != nil || note != "from the template" {
		t.Fatalf("copied content = %q (%v), want the template's row", note, err)
	}

	// An existing database is never re-templated, however the entry reads.
	again, err := service.AppDatabases(ctx, "demo", map[string]config.PgDBSpec{"DB_MAIN": {Database: target, Template: template}})
	if err != nil || again["DB_MAIN"] != env["DB_MAIN"] {
		t.Fatalf("second call = %v, %v", again, err)
	}
}

// TestAppDatabasesRefusesABusyTemplate is the regression for the failure operators actually hit:
// PostgreSQL copies a template only while nothing is connected to it.
func TestAppDatabasesRefusesABusyTemplate(t *testing.T) {
	service := New(config.Config{StateDir: t.TempDir(), Postgres: config.Postgres{Enabled: true}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	service.detect(ctx)
	if service.connConfig == nil {
		t.Skip("no reachable PostgreSQL server")
	}
	stamp := time.Now().UnixNano()
	template := fmt.Sprintf("dboss_busy_tpl_%d", stamp)
	target := fmt.Sprintf("dboss_busy_copy_%d", stamp)
	defer func() {
		_ = service.dropDatabase(context.Background(), service.connConfig, target)
		_ = service.dropDatabase(context.Background(), service.connConfig, template)
	}()
	if err := service.createDatabase(ctx, service.connConfig, template, ""); err != nil {
		t.Fatal(err)
	}

	holder, err := pgx.Connect(ctx, databaseURL(service.connConfig, template))
	if err != nil {
		t.Fatal(err)
	}
	entry := map[string]config.PgDBSpec{"DB_MAIN": {Database: target, Template: template}}
	_, err = service.AppDatabases(ctx, "demo", entry)
	if err == nil {
		_ = holder.Close(ctx)
		t.Fatal("a template with a live session should refuse the copy")
	}
	for _, want := range []string{template, "pg_terminate_backend"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}
	_ = holder.Close(ctx)

	// With the session gone the same call succeeds, so the failure is transient, not poisoned.
	if _, err := service.AppDatabases(ctx, "demo", entry); err != nil {
		t.Fatalf("after closing the session: %v", err)
	}
}

// TestAppDatabasesMissingTemplate names the config, not a bare SQLSTATE, and creates nothing.
func TestAppDatabasesMissingTemplate(t *testing.T) {
	service := New(config.Config{StateDir: t.TempDir(), Postgres: config.Postgres{Enabled: true}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	service.detect(ctx)
	if service.connConfig == nil {
		t.Skip("no reachable PostgreSQL server")
	}
	stamp := time.Now().UnixNano()
	template := fmt.Sprintf("dboss_absent_tpl_%d", stamp)
	target := fmt.Sprintf("dboss_absent_copy_%d", stamp)
	defer func() { _ = service.dropDatabase(context.Background(), service.connConfig, target) }()

	_, err := service.AppDatabases(ctx, "demo", map[string]config.PgDBSpec{"DB_MAIN": {Database: target, Template: template}})
	if err == nil || !strings.Contains(err.Error(), "template database") || !strings.Contains(err.Error(), template) {
		t.Fatalf("missing template error = %v", err)
	}
	exists, err := service.databaseExists(ctx, service.connConfig, target)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("a failed template create must not leave the database behind")
	}
}

// TestAppDatabasesDetectsOnDemand is the regression for a daemon restart taking every pg_db app
// down with it: detection runs off the start path, but super.New spawns autostart apps before the
// module manager runs, so the first caller has to be able to resolve the server itself.
func TestAppDatabasesDetectsOnDemand(t *testing.T) {
	service := New(config.Config{StateDir: t.TempDir(), Postgres: config.Postgres{Enabled: true}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Deliberately no detect() and no Start(): this is the state the supervisor finds at boot.
	if service.connConfig != nil {
		t.Fatal("a fresh service should hold no connection")
	}
	probe := New(config.Config{StateDir: t.TempDir(), Postgres: config.Postgres{Enabled: true}}, nil)
	probe.detect(ctx)
	if probe.connConfig == nil {
		t.Skip("no reachable PostgreSQL server")
	}

	database := fmt.Sprintf("dboss_ondemand_%d", time.Now().UnixNano())
	defer func() { _ = service.dropDatabase(context.Background(), probe.connConfig, database) }()
	env, err := service.AppDatabases(ctx, "demo", map[string]config.PgDBSpec{"DB_MAIN": {Database: database}})
	if err != nil {
		t.Fatalf("a never-started service should still resolve: %v", err)
	}
	if !strings.Contains(env["DB_MAIN"], database) {
		t.Fatalf("DB_MAIN = %q, want the database in it", env["DB_MAIN"])
	}
}
