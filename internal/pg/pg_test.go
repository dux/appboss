package pg

import (
	"errors"
	"strings"
	"testing"

	"dboss/internal/config"

	"github.com/jackc/pgx/v5/pgconn"
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
	backup := config.PostgresBackup{
		Databases: map[string]config.DatabaseBackup{
			"reports": {Rotation: "month"},
			"app":     {Rotation: "week"},
		},
	}
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

func TestCreateStatement(t *testing.T) {
	if got, want := createStatement("pr222_erpx", ""), `CREATE DATABASE "pr222_erpx"`; got != want {
		t.Errorf("createStatement = %q, want %q", got, want)
	}
	// Each name is its own identifier: pgx.Identifier{a, b} would render a qualified "a"."b".
	if got, want := createStatement("pr222_erpx", "template_erpx"), `CREATE DATABASE "pr222_erpx" TEMPLATE "template_erpx"`; got != want {
		t.Errorf("createStatement = %q, want %q", got, want)
	}
	if got := createStatement(`ev"il`, `t"pl`); !strings.Contains(got, `"ev""il"`) || !strings.Contains(got, `"t""pl"`) {
		t.Errorf("both identifiers should be sanitised, got %q", got)
	}
}

func TestCreateTemplateError(t *testing.T) {
	busy := createTemplateError("pr222_erpx", "template_erpx", &pgconn.PgError{Code: "55006"})
	// The message has to carry the way out; the server's own text names none.
	for _, want := range []string{"template_erpx", "pg_terminate_backend", "start the app again"} {
		if !strings.Contains(busy.Error(), want) {
			t.Errorf("busy-template error is missing %q: %v", want, busy)
		}
	}
	missing := createTemplateError("pr222_erpx", "template_erpx", &pgconn.PgError{Code: "3D000"})
	if !strings.Contains(missing.Error(), "does not exist") {
		t.Errorf("missing-template error = %v", missing)
	}
	other := createTemplateError("pr222_erpx", "template_erpx", errors.New("boom"))
	if !strings.Contains(other.Error(), "boom") {
		t.Errorf("an unmapped error should be wrapped, got %v", other)
	}
}
