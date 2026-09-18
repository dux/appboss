package metrics

import (
	"strings"
	"testing"
	"time"

	"app-boss/internal/res"
	"app-boss/internal/super"
)

func TestRenderIncludesCoreMetrics(t *testing.T) {
	now := time.Now()
	apps := []super.Snapshot{{
		Name:      "web",
		State:     super.Running,
		Autostart: true,
		Processes: []super.ProcessSnapshot{{Name: "web", State: super.Running, StartedAt: now.Add(-90 * time.Second), Restarts: 2, MemoryBytes: 1024}},
		Resources: res.Stats{MemoryBytes: 2048, CPUPercent: 3.5},
		RequestRates: super.RequestRates{
			LastMinute: 5,
			LastHour:   50,
			LastDay:    500,
		},
		Cron:  []super.CronSnapshot{{Name: "cleanup", LastEnd: now, LastExit: 1}},
		Hooks: []super.HookSnapshot{{Name: "deploy", LastEnd: now, LastExit: 0}},
	}}
	out := Render(apps, now, NotifyStats{Sent: 4, Failed: 1, Dropped: 2}, map[string]Latency{"web": {Count: 10, P50: 5, P95: 40, P99: 90}})
	for _, want := range []string{
		"# TYPE appboss_app_up gauge",
		`appboss_app_up{app="web"} 1`,
		"# TYPE appboss_app_state gauge",
		`appboss_app_state{app="web"} 2`,
		`appboss_app_uptime_seconds{app="web"} 90`,
		`appboss_app_memory_bytes{app="web"} 2048`,
		`appboss_app_cpu_percent{app="web"} 3.5`,
		`appboss_app_process_restarts_total{app="web",process="web"} 2`,
		`appboss_app_process_memory_bytes{app="web",process="web"} 1024`,
		`appboss_request_rate{app="web",window="minute"} 5`,
		`appboss_request_rate{app="web",window="hour"} 50`,
		`appboss_request_rate{app="web",window="day"} 500`,
		`appboss_request_duration_ms{app="web",quantile="0.5"} 5`,
		`appboss_request_duration_ms{app="web",quantile="0.95"} 40`,
		`appboss_request_duration_ms{app="web",quantile="0.99"} 90`,
		`appboss_request_duration_ms_samples{app="web"} 10`,
		`appboss_cron_last_exit{app="web",job="cleanup"} 1`,
		`appboss_hook_last_exit{app="web",hook="deploy"} 0`,
		`appboss_notifications_total{result="sent"} 4`,
		`appboss_notifications_total{result="failed"} 1`,
		`appboss_notifications_total{result="dropped"} 2`,
		`appboss_build_info{version="`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestRenderOmitsUnstartedJobsAndEscapesLabels(t *testing.T) {
	out := Render([]super.Snapshot{{
		Name:  `bad"name`,
		State: super.Stopped,
		Cron:  []super.CronSnapshot{{Name: "never"}},
		Hooks: []super.HookSnapshot{{Name: "never"}},
	}}, time.Now(), NotifyStats{}, nil)
	if strings.Contains(out, "appboss_cron_last_exit{") || strings.Contains(out, "appboss_hook_last_exit{") {
		t.Fatalf("a job that never ran should have no sample:\n%s", out)
	}
	if !strings.Contains(out, `appboss_app_up{app="bad\"name"} 0`) {
		t.Fatalf("label was not escaped:\n%s", out)
	}
}
