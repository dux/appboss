package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAppReadsCron(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	app, err := ParseApp([]byte(`procfile:
  web: ./server
cron:
  cleanup:
    schedule: every 6h
    command: bundle exec rake cleanup
    timeout: 30m
  report:
    schedule: "0 7 * * 1-5"
    command: bundle exec rake report
    overlap: true
`), path, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Cron) != 2 {
		t.Fatalf("cron = %+v", app.Cron)
	}
	cleanup := app.Cron["cleanup"]
	if cleanup.Schedule != "every 6h" || cleanup.Command != "bundle exec rake cleanup" || cleanup.Timeout.Value() != 30*time.Minute || cleanup.Overlap {
		t.Fatalf("cleanup = %+v", cleanup)
	}
	if report := app.Cron["report"]; !report.Overlap || report.Schedule != "0 7 * * 1-5" {
		t.Fatalf("report = %+v", report)
	}
}

func TestCronValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	base := "procfile:\n  web: ./server\ncron:\n  job:\n"
	for name, body := range map[string]string{
		"bad schedule":   "    schedule: sometimes\n    command: ./run\n",
		"empty command":  "    schedule: every 5m\n    command: \"  \"\n",
		"negative time":  "    schedule: every 5m\n    command: ./run\n    timeout: -1m\n",
		"unknown option": "    schedule: every 5m\n    command: ./run\n    retries: 3\n",
	} {
		if _, err := ParseApp([]byte(base+body), path, Default().Defaults); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := ParseApp([]byte(base+"    schedule: every 5m\n    command: ./run\n"), path, Default().Defaults); err != nil {
		t.Fatalf("valid cron rejected: %v", err)
	}
}

func TestCronNameRejectsBadNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	_, err := ParseApp([]byte("procfile:\n  web: ./server\ncron:\n  Bad Name:\n    schedule: every 5m\n    command: ./run\n"), path, Default().Defaults)
	if err == nil || !strings.Contains(err.Error(), "invalid job name") {
		t.Fatalf("err = %v", err)
	}
}
