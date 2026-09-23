package supervisor

import (
	"time"

	"dboss/internal/config"
	"dboss/internal/res"
)

type State string

const (
	Stopped  State = "stopped"
	Starting State = "starting"
	Running  State = "running"
	Stopping State = "stopping"
	Crashed  State = "crashed"
)

type ProcessSnapshot struct {
	Name        string    `json:"name"`
	Command     string    `json:"command"`
	State       State     `json:"state"`
	PID         int       `json:"pid,omitempty"`
	Port        int       `json:"port"`
	Restarts    int       `json:"restarts"`
	MemoryBytes int64     `json:"memory_bytes,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
}

type RequestRates struct {
	LastMinute int64 `json:"last_minute"`
	LastHour   int64 `json:"last_hour"`
	LastDay    int64 `json:"last_day"`
}

// DiskUsage is what an app occupies on disk: its own directory and the log store dboss writes for
// it. It is measured in the background and filled in by ops, so a zero MeasuredAt means the app
// has not been measured yet rather than that it is empty.
type DiskUsage struct {
	AppBytes   int64     `json:"app_bytes"`
	LogBytes   int64     `json:"log_bytes"`
	TotalBytes int64     `json:"total_bytes"`
	MeasuredAt time.Time `json:"measured_at,omitempty"`
}

// WebProcessSnapshot is one web process of an app: the hosts it answers and the canonical host
// its other hosts redirect to, plus its realtime hub.
type WebProcessSnapshot struct {
	Name          string        `json:"name"`
	Hosts         []string      `json:"hosts"`
	CanonicalHost string        `json:"canonical_host,omitempty"`
	Static        string        `json:"static"`
	Pubsub        config.Pubsub `json:"pubsub"`
}

// ExecResult is the combined output and exit code of a one-off command.
type ExecResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

type Snapshot struct {
	Name            string               `json:"name"`
	State           State                `json:"state"`
	Maintenance     bool                 `json:"maintenance"`
	Draining        bool                 `json:"draining,omitempty"`
	Dir             string               `json:"dir"`
	Hosts           []string             `json:"hosts"`
	WebProcesses    []WebProcessSnapshot `json:"web_processes"`
	Autostart       bool                 `json:"autostart"`
	Deletable       bool                 `json:"deletable"`
	WakeButton      bool                 `json:"wake_button,omitempty"`
	Web             config.Web           `json:"web"`
	Processes       []ProcessSnapshot    `json:"processes"`
	Cron            []CronSnapshot       `json:"cron,omitempty"`
	Hooks           []HookSnapshot       `json:"hooks,omitempty"`
	LastActivity    time.Time            `json:"last_activity,omitempty"`
	Uptime          string               `json:"uptime,omitempty"`
	Resources       res.Stats            `json:"resources"`
	RequestRates    RequestRates         `json:"request_rates"`
	Disk            DiskUsage            `json:"disk"`
	Error           string               `json:"error,omitempty"`
	ErrorLog        []string             `json:"error_log,omitempty"`
	LogRetention    time.Duration        `json:"-"`
	StdoutRetention time.Duration        `json:"-"`
	TmpClean        time.Duration        `json:"-"`
}

// Serving reports whether a request for the app would be answered by the app itself: it runs, or
// it is stopped and the proxy wakes it on the next request. Button apps only wake on a POST.
func (s Snapshot) Serving() bool {
	if s.Draining || s.Maintenance {
		return false
	}
	return s.State == Running || (s.State == Stopped && !s.WakeButton)
}

// WebForHost returns the web process whose hosts best match host. A request that resolved to an
// app is always served by exactly one of its web processes, and the longest pattern wins.
func (s Snapshot) WebForHost(host string) (WebProcessSnapshot, bool) {
	host = config.NormalizeHost(host)
	bestScore := -1
	var best WebProcessSnapshot
	for _, web := range s.WebProcesses {
		if score, ok := config.BestMatch(host, web.Hosts); ok && score > bestScore {
			best, bestScore = web, score
		}
	}
	return best, bestScore >= 0
}

// WebProcessSnapshots converts the derived config web processes into the snapshot shape.
func WebProcessSnapshots(webs []config.WebProcess) []WebProcessSnapshot {
	result := make([]WebProcessSnapshot, 0, len(webs))
	for _, web := range webs {
		result = append(result, WebProcessSnapshot{Name: web.Name, Hosts: web.Hosts, CanonicalHost: web.CanonicalHost, Static: web.Static, Pubsub: web.Pubsub})
	}
	return result
}
