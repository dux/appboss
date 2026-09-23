package metrics

import (
	"strings"
	"testing"
	"time"

	"dboss/internal/logstore"
	"dboss/internal/notify"
	"dboss/internal/pubsub"
	"dboss/internal/res"
	"dboss/internal/supervisor"
)

func TestRenderIncludesCoreMetrics(t *testing.T) {
	now := time.Now()
	apps := []supervisor.Snapshot{{
		Name:      "web",
		State:     supervisor.Running,
		Autostart: true,
		Processes: []supervisor.ProcessSnapshot{{Name: "web", State: supervisor.Running, StartedAt: now.Add(-90 * time.Second), Restarts: 2, MemoryBytes: 1024}},
		Resources: res.Stats{MemoryBytes: 2048, CPUPercent: 3.5},
		RequestRates: supervisor.RequestRates{
			LastMinute: 5,
			LastHour:   50,
			LastDay:    500,
		},
		Disk:  supervisor.DiskUsage{AppBytes: 4096, LogBytes: 512, TotalBytes: 4608, MeasuredAt: now},
		Cron:  []supervisor.CronSnapshot{{Name: "cleanup", LastEnd: now, LastExit: 1}},
		Hooks: []supervisor.HookSnapshot{{Name: "deploy", LastEnd: now, LastExit: 0}},
	}}
	out := Render(Input{
		Apps:    apps,
		Now:     now,
		Notify:  notify.Stats{Sent: 4, Failed: 1, Dropped: 2},
		Latency: map[string]logstore.Window{"web": {Count: 10, P50: 5, P95: 40, P99: 90}},
		Pubsub:  map[string]pubsub.Stats{"web": {Clients: 3, Channels: 2, Messages: 11}},
	})
	for _, want := range []string{
		"# TYPE dboss_app_up gauge",
		`dboss_app_up{app="web"} 1`,
		"# TYPE dboss_app_state gauge",
		`dboss_app_state{app="web"} 2`,
		`dboss_app_uptime_seconds{app="web"} 90`,
		`dboss_app_memory_bytes{app="web"} 2048`,
		`dboss_app_cpu_percent{app="web"} 3.5`,
		`dboss_app_disk_bytes{app="web",part="app"} 4096`,
		`dboss_app_disk_bytes{app="web",part="logs"} 512`,
		"# TYPE dboss_app_disk_measured_timestamp_seconds gauge",
		`dboss_app_process_restarts_total{app="web",process="web"} 2`,
		`dboss_app_process_memory_bytes{app="web",process="web"} 1024`,
		`dboss_request_rate{app="web",window="minute"} 5`,
		`dboss_request_rate{app="web",window="hour"} 50`,
		`dboss_request_rate{app="web",window="day"} 500`,
		`dboss_request_duration_ms{app="web",quantile="0.5"} 5`,
		`dboss_request_duration_ms{app="web",quantile="0.95"} 40`,
		`dboss_request_duration_ms{app="web",quantile="0.99"} 90`,
		`dboss_cron_last_exit{app="web",job="cleanup"} 1`,
		`dboss_hook_last_exit{app="web",hook="deploy"} 0`,
		`dboss_notifications_total{result="sent"} 4`,
		`dboss_notifications_total{result="failed"} 1`,
		`dboss_notifications_total{result="dropped"} 2`,
		`dboss_pubsub_clients{app="web"} 3`,
		`dboss_pubsub_channels{app="web"} 2`,
		`dboss_pubsub_messages_total{app="web"} 11`,
		`dboss_build_info{version="`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestRenderOmitsUnstartedJobsAndEscapesLabels(t *testing.T) {
	out := Render(Input{Now: time.Now(), Apps: []supervisor.Snapshot{{
		Name:  `bad"name`,
		State: supervisor.Stopped,
		Cron:  []supervisor.CronSnapshot{{Name: "never"}},
		Hooks: []supervisor.HookSnapshot{{Name: "never"}},
	}}})
	if strings.Contains(out, "dboss_cron_last_exit{") || strings.Contains(out, "dboss_hook_last_exit{") {
		t.Fatalf("a job that never ran should have no sample:\n%s", out)
	}
	// An app the daily walk has not reached yet is a gap, not zero bytes.
	if strings.Contains(out, "dboss_app_disk_bytes{") || strings.Contains(out, "dboss_app_disk_measured_timestamp_seconds{") {
		t.Fatalf("an unmeasured app should have no disk sample:\n%s", out)
	}
	if !strings.Contains(out, `dboss_app_up{app="bad\"name"} 0`) {
		t.Fatalf("label was not escaped:\n%s", out)
	}
}
