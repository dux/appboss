package pg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if got := dumpName(at); got != "BACKUP_2026-09-19T14-30-05Z.zip" {
		t.Fatalf("dumpName = %q", got)
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

func TestCatalogRefusesToOverwriteAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pg-backups.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o640); err != nil {
		t.Fatal(err)
	}
	catalog := newCatalog(dir)
	if err := catalog.record(Backup{ID: "new", Database: "app", Status: "ok"}); err == nil {
		t.Fatal("record saved over an unreadable catalog")
	}
	if data, _ := os.ReadFile(path); string(data) != "{broken" {
		t.Fatalf("catalog file was rewritten: %q", data)
	}
}

func TestDropRefusesReservedDatabasesOnEveryPath(t *testing.T) {
	connConfig := connConfig(t, "postgres://app@127.0.0.1:1/app")
	for _, name := range []string{"postgres", "template0", "template1"} {
		if err := dropDatabase(context.Background(), connConfig, name); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("dropDatabase(%s) = %v, want reserved refusal", name, err)
		}
	}
}
