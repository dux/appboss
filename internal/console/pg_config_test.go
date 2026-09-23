package console

import (
	"strings"
	"testing"

	"dboss/internal/config"

	"gopkg.in/yaml.v3"
)

func TestPatchPostgresBackupKeepsOtherKeys(t *testing.T) {
	contents := "apps: ./apps\nproxy:\n  listen: \":80\"\npostgres:\n  dsn: $DATABASE_URL\n  backups:\n    old: week\n"
	patched, err := patchPostgresBackup(contents, config.PostgresBackups{"app_production": "month"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.Parse([]byte(patched), "/srv/dboss.yaml")
	if err != nil {
		t.Fatalf("patched config no longer parses: %v\n%s", err, patched)
	}
	if parsed.Proxy.Listen[0] != ":80" {
		t.Fatalf("proxy listen was lost: %s", patched)
	}
	if !parsed.Postgres.Enabled || parsed.Postgres.DSN != "$DATABASE_URL" {
		t.Fatalf("postgres keys outside backup were lost:\n%s", patched)
	}
	if len(parsed.Postgres.Backups) != 1 || parsed.Postgres.Backups.Rotation("app_production") != "month" {
		t.Fatalf("backup block was not replaced:\n%s", patched)
	}
	if selected := parsed.Postgres.Backups.Selected(); len(selected) != 1 || selected[0] != "app_production" {
		t.Fatalf("database selection lost:\n%s", patched)
	}
}

func TestPatchPostgresBackupCreatesBlock(t *testing.T) {
	patched, err := patchPostgresBackup("apps: ./apps\n", config.PostgresBackups{"reports": "month"})
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := yaml.Unmarshal([]byte(patched), &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["postgres"]; !ok {
		t.Fatalf("postgres block was not created:\n%s", patched)
	}
	if !strings.Contains(patched, "reports: month") {
		t.Fatalf("backup rotation missing:\n%s", patched)
	}
}
