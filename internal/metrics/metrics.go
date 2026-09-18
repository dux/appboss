// Package metrics renders the daemon's state as Prometheus text. It reads the same app snapshots
// the console shows, so a metric can never disagree with the UI.
package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"app-boss/internal/super"
	"app-boss/internal/version"
)

// stateValue maps an app state to the number used by appboss_app_state.
var stateValue = map[super.State]int{
	super.Stopped:  0,
	super.Starting: 1,
	super.Running:  2,
	super.Stopping: 3,
	super.Crashed:  4,
}

// Render writes the Prometheus exposition for apps, already sorted by name.
func Render(apps []super.Snapshot, now time.Time) string {
	var b strings.Builder
	metric := func(name, help, kind string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	sample := func(name, labels string, value float64) {
		fmt.Fprintf(&b, "%s%s %s\n", name, labels, strconv.FormatFloat(value, 'g', -1, 64))
	}

	name := func(app string) string { return "{app=" + quote(app) + "}" }

	metric("appboss_build_info", "Build information.", "gauge")
	sample("appboss_build_info", `{version=`+quote(version.String())+`}`, 1)

	metric("appboss_app_up", "1 when the app is running, 0 otherwise.", "gauge")
	for _, app := range apps {
		up := 0.0
		if app.State == super.Running {
			up = 1
		}
		sample("appboss_app_up", name(app.Name), up)
	}

	metric("appboss_app_state", "App state: 0 stopped, 1 starting, 2 running, 3 stopping, 4 crashed.", "gauge")
	for _, app := range apps {
		sample("appboss_app_state", name(app.Name), float64(stateValue[app.State]))
	}

	metric("appboss_app_uptime_seconds", "Seconds since the app's earliest running process started.", "gauge")
	for _, app := range apps {
		if started, ok := uptimeStart(app); ok {
			sample("appboss_app_uptime_seconds", name(app.Name), now.Sub(started).Seconds())
		}
	}

	metric("appboss_app_memory_bytes", "Resident memory of every app process (approximate on the procgroup backend).", "gauge")
	for _, app := range apps {
		sample("appboss_app_memory_bytes", name(app.Name), float64(app.Resources.MemoryBytes))
	}

	metric("appboss_app_cpu_percent", "CPU use of every app process, percent of one core (approximate on the procgroup backend).", "gauge")
	for _, app := range apps {
		sample("appboss_app_cpu_percent", name(app.Name), app.Resources.CPUPercent)
	}

	metric("appboss_app_process_restarts_total", "Restarts of one process.", "counter")
	for _, app := range apps {
		for _, process := range app.Processes {
			labels := "{app=" + quote(app.Name) + ",process=" + quote(process.Name) + "}"
			sample("appboss_app_process_restarts_total", labels, float64(process.Restarts))
		}
	}

	metric("appboss_app_process_memory_bytes", "Resident memory of one process (approximate on the procgroup backend).", "gauge")
	for _, app := range apps {
		for _, process := range app.Processes {
			if process.MemoryBytes == 0 {
				continue
			}
			labels := "{app=" + quote(app.Name) + ",process=" + quote(process.Name) + "}"
			sample("appboss_app_process_memory_bytes", labels, float64(process.MemoryBytes))
		}
	}

	metric("appboss_request_rate", "Proxied requests counted over the window.", "gauge")
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
			sample("appboss_request_rate", labels, float64(window.value))
		}
	}

	metric("appboss_cron_last_exit", "Exit code of the last cron run.", "gauge")
	for _, app := range apps {
		for _, job := range app.Cron {
			if job.LastEnd.IsZero() {
				continue
			}
			labels := "{app=" + quote(app.Name) + ",job=" + quote(job.Name) + "}"
			sample("appboss_cron_last_exit", labels, float64(job.LastExit))
		}
	}

	metric("appboss_hook_last_exit", "Exit code of the last deploy hook run.", "gauge")
	for _, app := range apps {
		for _, hook := range app.Hooks {
			if hook.LastEnd.IsZero() {
				continue
			}
			labels := "{app=" + quote(app.Name) + ",hook=" + quote(hook.Name) + "}"
			sample("appboss_hook_last_exit", labels, float64(hook.LastExit))
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
