package pg

import (
	"testing"

	"app-boss/internal/config"
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

func TestParseEndpoint(t *testing.T) {
	for _, test := range []struct {
		in     string
		host   string
		secure bool
	}{
		{"https://account.r2.cloudflarestorage.com/", "account.r2.cloudflarestorage.com", true},
		{"http://minio.local:9000", "minio.local:9000", false},
		{"s3.amazonaws.com", "s3.amazonaws.com", true},
	} {
		host, secure := parseEndpoint(test.in)
		if host != test.host || secure != test.secure {
			t.Errorf("parseEndpoint(%q) = %q, %t; want %q, %t", test.in, host, secure, test.host, test.secure)
		}
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

func TestDestinationsApplyOverride(t *testing.T) {
	no := false
	backup := config.PostgresBackup{Dir: "/dumps", S3: true, Databases: map[string]config.Target{
		"both":   {},
		"local":  {S3: &no},
		"s3only": {Local: &no},
	}}
	local, s3 := backup.Destinations("both")
	if !local || !s3 {
		t.Fatalf("empty target should follow defaults, got local=%t s3=%t", local, s3)
	}
	if local, s3 := backup.Destinations("local"); !local || s3 {
		t.Fatalf("s3: false should disable s3, got local=%t s3=%t", local, s3)
	}
	if local, s3 := backup.Destinations("s3only"); local || !s3 {
		t.Fatalf("local: false should disable local, got local=%t s3=%t", local, s3)
	}
}
