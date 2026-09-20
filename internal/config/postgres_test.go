package config

import (
	"strings"
	"testing"
)

func TestPostgresBackupValidation(t *testing.T) {
	base := "apps: ./apps\n"
	for _, test := range []struct {
		name string
		data string
		want string
	}{
		{"valid", "postgres:\n  backup:\n    at: \"04:00\"\n    days: 30\n    databases:\n      app_production: {}\n", ""},
		{"missing dir", "postgres:\n  backup:\n    dir: \"\"\n    databases:\n      app_production: {}\n", "a directory is required"},
		{"bad name", "postgres:\n  backup:\n    databases:\n      \"app-production\": {}\n", "invalid database name"},
		{"bad time", "postgres:\n  backup:\n    at: \"25:00\"\n", "must be a UTC time HH:MM"},
		{"bad days", "postgres:\n  backup:\n    days: -1\n", "backup.days"},
		{"bad db days", "postgres:\n  backup:\n    databases:\n      app_production:\n        days: -1\n", "backup.databases.app_production.days"},
	} {
		_, err := Parse([]byte(base+test.data), "/srv/dboss.yaml")
		if test.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", test.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: got %v, want %q", test.name, err, test.want)
		}
	}
}

func TestPostgresIsHostOnly(t *testing.T) {
	_, err := ParseApp([]byte("procfile:\n  web: ./x\npostgres:\n  dsn: \"\"\n"), "/srv/apps/demo/dboss.yaml", Default().Defaults)
	if err == nil || !strings.Contains(err.Error(), "postgres") || !strings.Contains(err.Error(), "only valid in the root") {
		t.Fatalf("postgres must be rejected in an app file, got %v", err)
	}
}
