// Package metrics renders the daemon's state as Prometheus text. It reads the same app snapshots
// the console shows, so a metric can never disagree with the UI.
package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"dboss/internal/notify"
	"dboss/internal/super"
	"dboss/internal/version"
)

// stateValue maps an app state to the number used by dboss_app_state.
var stateValue = map[super.State]int{
	super.Stopped:  0,
	super.Starting: 1,
	super.Running:  2,
	super.Stopping: 3,
	super.Crashed:  4,
}

// NotifyStats is the notifier's counter set; the alias keeps the handler's signature stable.
type NotifyStats = notify.Stats

// Latency is the request duration summary of one app, in milliseconds.
type Latency struct {
	Count int
	P50   float64
	P95   float64
	P99   float64
}

// PGStats is the PostgreSQL state the metrics endpoint exposes: whether the server is reachable,
// per-database sizes and the age of the last successful backup.
type PGStats struct {
	Up        bool
	Databases []PGDatabase
	Backups   []PGBackup
}

// PGDatabase is one database and its size.
type PGDatabase struct {
	Name      string
	SizeBytes int64
}

// PGBackup is one catalog row reduced to what a dashboard needs.
type PGBackup struct {
	Database string
	Time     time.Time
	Status   string
}

// PubsubStats is the realtime hub state of one app.
type PubsubStats struct {
	Clients  int
	Channels int
	Messages uint64
}

// Render writes the Prometheus exposition for apps, the notifier counters, the request latency
// and the PostgreSQL state, already sorted by name.
func Render(apps []super.Snapshot, now time.Time, notify NotifyStats, latency map[string]Latency, postgres PGStats, pubsub map[string]PubsubStats) string {
	var b strings.Builder
	metric := func(name, help, kind string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	sample := func(name, labels string, value float64) {
		fmt.Fprintf(&b, "%s%s %s\n", name, labels, strconv.FormatFloat(value, 'g', -1, 64))
	}

	name := func(app string) string { return "{app=" + quote(app) + "}" }

	metric("dboss_build_info", "Build information.", "gauge")
	sample("dboss_build_info", `{version=`+quote(version.String())+`}`, 1)

	metric("dboss_app_up", "1 when the app is running, 0 otherwise.", "gauge")
	for _, app := range apps {
		up := 0.0
		if app.State == super.Running {
			up = 1
		}
		sample("dboss_app_up", name(app.Name), up)
	}

	metric("dboss_app_state", "App state: 0 stopped, 1 starting, 2 running, 3 stopping, 4 crashed.", "gauge")
	for _, app := range apps {
		sample("dboss_app_state", name(app.Name), float64(stateValue[app.State]))
	}

	metric("dboss_app_uptime_seconds", "Seconds since the app's earliest running process started.", "gauge")
	for _, app := range apps {
		if started, ok := uptimeStart(app); ok {
			sample("dboss_app_uptime_seconds", name(app.Name), now.Sub(started).Seconds())
		}
	}

	metric("dboss_app_memory_bytes", "Resident memory of every app process (approximate on the procgroup backend).", "gauge")
	for _, app := range apps {
		sample("dboss_app_memory_bytes", name(app.Name), float64(app.Resources.MemoryBytes))
	}

	metric("dboss_app_cpu_percent", "CPU use of every app process, percent of one core (approximate on the procgroup backend).", "gauge")
	for _, app := range apps {
		sample("dboss_app_cpu_percent", name(app.Name), app.Resources.CPUPercent)
	}

	metric("dboss_app_process_restarts_total", "Restarts of one process.", "counter")
	for _, app := range apps {
		for _, process := range app.Processes {
			labels := "{app=" + quote(app.Name) + ",process=" + quote(process.Name) + "}"
			sample("dboss_app_process_restarts_total", labels, float64(process.Restarts))
		}
	}

	metric("dboss_app_process_memory_bytes", "Resident memory of one process (approximate on the procgroup backend).", "gauge")
	for _, app := range apps {
		for _, process := range app.Processes {
			if process.MemoryBytes == 0 {
				continue
			}
			labels := "{app=" + quote(app.Name) + ",process=" + quote(process.Name) + "}"
			sample("dboss_app_process_memory_bytes", labels, float64(process.MemoryBytes))
		}
	}

	metric("dboss_request_rate", "Proxied requests counted over the window.", "gauge")
	for _, app := range apps {
		for _, window := range []struct {
			label string
			value int64
		}{
			{"minute", app.RequestRates.LastMinute},
			{"hour", app.RequestRates.LastHour},
			{"day", app.RequestRates.LastDay},
		} {
			labels := "{app=" + quote(app.Name) + ",window=" + quote(window.label) + "}"
			sample("dboss_request_rate", labels, float64(window.value))
		}
	}

	metric("dboss_cron_last_exit", "Exit code of the last cron run.", "gauge")
	for _, app := range apps {
		for _, job := range app.Cron {
			if job.LastEnd.IsZero() {
				continue
			}
			labels := "{app=" + quote(app.Name) + ",job=" + quote(job.Name) + "}"
			sample("dboss_cron_last_exit", labels, float64(job.LastExit))
		}
	}

	metric("dboss_hook_last_exit", "Exit code of the last deploy hook run.", "gauge")
	for _, app := range apps {
		for _, hook := range app.Hooks {
			if hook.LastEnd.IsZero() {
				continue
			}
			labels := "{app=" + quote(app.Name) + ",hook=" + quote(hook.Name) + "}"
			sample("dboss_hook_last_exit", labels, float64(hook.LastExit))
		}
	}

	metric("dboss_notifications_total", "Operator webhook sends by result.", "counter")
	sample("dboss_notifications_total", `{result="sent"}`, float64(notify.Sent))
	sample("dboss_notifications_total", `{result="failed"}`, float64(notify.Failed))
	sample("dboss_notifications_total", `{result="dropped"}`, float64(notify.Dropped))

	metric("dboss_request_duration_ms", "Request duration over the last hour, milliseconds, by quantile.", "gauge")
	for _, app := range apps {
		stats := latency[app.Name]
		if stats.Count == 0 {
			continue
		}
		for _, quantile := range []struct {
			label string
			value float64
		}{{"0.5", stats.P50}, {"0.95", stats.P95}, {"0.99", stats.P99}} {
			labels := "{app=" + quote(app.Name) + ",quantile=" + quote(quantile.label) + "}"
			sample("dboss_request_duration_ms", labels, quantile.value)
		}
	}
	metric("dboss_request_duration_ms_samples", "Requests sampled for the duration quantiles.", "gauge")
	for _, app := range apps {
		sample("dboss_request_duration_ms_samples", name(app.Name), float64(latency[app.Name].Count))
	}

	metric("dboss_pubsub_clients", "Subscribers connected to an app's realtime channels.", "gauge")
	for _, app := range apps {
		sample("dboss_pubsub_clients", name(app.Name), float64(pubsub[app.Name].Clients))
	}

	metric("dboss_pubsub_channels", "Realtime channels an app knows about.", "gauge")
	for _, app := range apps {
		sample("dboss_pubsub_channels", name(app.Name), float64(pubsub[app.Name].Channels))
	}

	metric("dboss_pubsub_messages_total", "Messages published to an app's realtime channels.", "counter")
	for _, app := range apps {
		sample("dboss_pubsub_messages_total", name(app.Name), float64(pubsub[app.Name].Messages))
	}

	metric("dboss_pg_up", "1 when the host PostgreSQL server is reachable, 0 otherwise.", "gauge")
	up := 0.0
	if postgres.Up {
		up = 1
	}
	sample("dboss_pg_up", "", up)

	metric("dboss_pg_database_size_bytes", "Size on disk of one database.", "gauge")
	for _, database := range postgres.Databases {
		sample("dboss_pg_database_size_bytes", "{database="+quote(database.Name)+"}", float64(database.SizeBytes))
	}

	metric("dboss_pg_backup_last_success_timestamp_seconds", "Unix time of the newest successful backup of one database.", "gauge")
	metric("dboss_pg_backup_count", "Recorded backups of one database by status.", "gauge")
	lastSuccess := map[string]float64{}
	counts := map[string]map[string]int{}
	for _, backup := range postgres.Backups {
		if counts[backup.Database] == nil {
			counts[backup.Database] = map[string]int{}
		}
		counts[backup.Database][backup.Status]++
		if backup.Status == "ok" {
			if moment := float64(backup.Time.Unix()); moment > lastSuccess[backup.Database] {
				lastSuccess[backup.Database] = moment
			}
		}
	}
	for database, moment := range lastSuccess {
		sample("dboss_pg_backup_last_success_timestamp_seconds", "{database="+quote(database)+"}", moment)
	}
	for database, byStatus := range counts {
		for status, count := range byStatus {
			sample("dboss_pg_backup_count", "{database="+quote(database)+",status="+quote(status)+"}", float64(count))
		}
	}

	return b.String()
}

// uptimeStart is the earliest start time among an app's running processes.
func uptimeStart(app super.Snapshot) (time.Time, bool) {
	var earliest time.Time
	for _, process := range app.Processes {
		if process.State != super.Running || process.StartedAt.IsZero() {
			continue
		}
		if earliest.IsZero() || process.StartedAt.Before(earliest) {
			earliest = process.StartedAt
		}
	}
	return earliest, !earliest.IsZero()
}

// quote escapes a Prometheus label value.
func quote(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + replacer.Replace(value) + `"`
}
