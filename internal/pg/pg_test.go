package pg

import (
	"testing"

	"dboss/internal/config"
)

func TestCandidateDSNs(t *testing.T) {
	if got := candidateDSNs("postgres://db@example.com/app"); len(got) != 1 || got[0] != "postgres://db@example.com/app" {
		t.Fatalf("explicit dsn should be used as is, got %v", got)
	}
	got := candidateDSNs("")
	if len(got) != 3 || got[0] != "host=/var/run/postgresql" || got[1] != "host=/tmp" {
		t.Fatalf("empty dsn should try the unix sockets first, got %v", got)
	}
}

func TestCatalogRoundTrip(t *testing.T) {
	dir := t.TempDir()
	catalog := newCatalog(dir)
	if err := catalog.record(Backup{ID: "a", Database: "app", Status: "ok", Time: "2026-09-19T04:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.record(Backup{ID: "b", Database: "app", Status: "ok", Time: "2026-09-19T06:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	reloaded := newCatalog(dir)
	list := reloaded.list()
	if len(list) != 2 || list[0].ID != "b" {
		t.Fatalf("catalog should list newest first, got %+v", list)
	}
	if err := reloaded.forget(map[string]bool{"b": true}); err != nil {
		t.Fatal(err)
	}
	if list := newCatalog(dir).list(); len(list) != 1 || list[0].ID != "a" {
		t.Fatalf("forget should drop b, got %+v", list)
	}
}

func TestSelectedDatabases(t *testing.T) {
	backup := config.PostgresBackups{"reports": "month", "app": ""}
	if got := backup.Selected(); len(got) != 2 || got[0] != "app" || got[1] != "reports" {
		t.Fatalf("Selected() = %v", got)
	}
	if got := backup.Rotation("reports"); got != "month" {
		t.Fatalf("Rotation(reports) = %q", got)
	}
	if got := backup.Rotation("app"); got != "week" {
		t.Fatalf("Rotation(app) = %q", got)
	}
	if got := backup.Rotation("missing"); got != "" {
		t.Fatalf("Rotation(missing) = %q, want empty", got)
	}
}
