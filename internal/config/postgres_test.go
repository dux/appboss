package config

import (
	"strings"
	"testing"
)

func TestPostgresBackupValidation(t *testing.T) {
	base := "apps: ./apps\n"
	s3 := "s3:\n  endpoint: https://account.r2.cloudflarestorage.com\n  region: auto\n  bucket: backups\n  access_key: key\n  secret_key: secret\n"
	for _, test := range []struct {
		name string
		data string
		want string
	}{
		{"valid", s3 + "postgres:\n  backup:\n    databases:\n      app_production: {}\n", ""},
		{"s3 missing", "postgres:\n  backup:\n    s3: true\n    databases:\n      app_production:\n        local: false\n", "s3 is not configured"},
		{"no destination", "postgres:\n  backup:\n    dir: \"\"\n    s3: false\n    databases:\n      app_production: {}\n", "neither a local nor an s3 destination"},
		{"bad name", "postgres:\n  backup:\n    databases:\n      \"app-production\": {}\n", "invalid database name"},
		{"bad keep", "postgres:\n  backup:\n    keep:\n      hourly: -1\n", "cannot be negative"},
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

func TestS3RequiresEndpointAndCredentials(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		want string
	}{
		{"endpoint only", "s3:\n  endpoint: https://x.example.com\n", "endpoint and bucket"},
		{"no credentials", "s3:\n  endpoint: https://x.example.com\n  region: auto\n  bucket: b\n", "access_key and secret_key are required"},
	} {
		_, err := Parse([]byte("apps: ./apps\n"+test.data), "/srv/dboss.yaml")
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
