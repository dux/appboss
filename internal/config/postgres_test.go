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
		{"valid", "postgres:\n  backup:\n    databases:\n      app_production: {rotation: week}\n      reports: {rotation: month}\n", ""},
		{"empty rotation", "postgres:\n  backup:\n    databases:\n      app_production: {}\n", ""},
		{"bad name", "postgres:\n  backup:\n    databases:\n      \"app-production\": {}\n", "invalid database name"},
		{"bad rotation", "postgres:\n  backup:\n    databases:\n      app_production: {rotation: daily}\n", "must be week or month"},
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
