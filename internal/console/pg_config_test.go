package console

import (
	"strings"
	"testing"
	"time"

	"dboss/internal/config"

	"gopkg.in/yaml.v3"
)

func TestPatchPostgresBackupKeepsOtherKeys(t *testing.T) {
	no := false
	contents := "apps: ./apps\nproxy:\n  listen: \":80\"\npostgres:\n  enabled: true\n  dsn: $DATABASE_URL\n  backup:\n    every: 6h\n"
	patched, err := patchPostgresBackup(contents, config.PostgresBackup{
		Dir:       "/var/backups/pg",
		S3:        true,
		Every:     config.Duration(6 * time.Hour),
		Databases: map[string]config.Target{"app_production": {S3: &no}},
	})
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
	if parsed.Postgres.Backup.Dir != "/var/backups/pg" || len(parsed.Postgres.Backup.Databases) != 1 {
		t.Fatalf("backup block was not replaced:\n%s", patched)
	}
	local, s3 := parsed.Postgres.Backup.Destinations("app_production")
	if !local || s3 {
		t.Fatalf("destination override lost: local=%t s3=%t", local, s3)
	}
}

func TestPatchPostgresBackupCreatesBlock(t *testing.T) {
	patched, err := patchPostgresBackup("apps: ./apps\n", config.PostgresBackup{Dir: "/dumps"})
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
	if !strings.Contains(patched, "dir: /dumps") {
		t.Fatalf("backup dir missing:\n%s", patched)
	}
}
