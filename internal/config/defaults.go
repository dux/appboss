package config

import (
	"net/netip"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults holds every app-level key. Process keys can be overridden again per process, Web
// keys apply to the app as a whole. Both groups are flattened in YAML and JSON.
type Defaults struct {
	Process `yaml:",inline"`
	Web     `yaml:",inline"`
}

type Process struct {
	IdleStop Duration `yaml:"idle_stop" json:"idle_stop"`
	// Health is resolved from the web process's procfile health path; it is not a settable key.
	Health             string            `yaml:"-" json:"-"`
	LivenessInterval   Duration          `yaml:"liveness_interval" json:"liveness_interval"`
	HealthTimeout      Duration          `yaml:"health_timeout" json:"health_timeout"`
	UnhealthyThreshold int               `yaml:"unhealthy_threshold" json:"unhealthy_threshold"`
	StopTimeout        Duration          `yaml:"stop_timeout" json:"stop_timeout"`
	StopSignal         string            `yaml:"stop_signal" json:"stop_signal"`
	Restart            string            `yaml:"restart" json:"restart"`
	MaxRestarts        int               `yaml:"max_restarts" json:"max_restarts"`
	LogRetention       Duration          `yaml:"log_retention" json:"log_retention"`
	StdoutRetention    Duration          `yaml:"stdout_retention" json:"stdout_retention"`
	TmpClean           Duration          `yaml:"tmp_clean" json:"tmp_clean"`
	Env                map[string]string `yaml:"env" json:"env"`
	MemoryMax          Size              `yaml:"memory_max" json:"memory_max"`
	CPUMax             int               `yaml:"cpu_max" json:"cpu_max"`
}

// Web drives the proxy in front of the app. BasicAuth never leaves the process as JSON so the
// hashes stay out of the console and `dboss status --json`.
type Web struct {
	HealthEndpoint   string            `yaml:"health_endpoint" json:"health_endpoint"`
	Static           StaticPath        `yaml:"static" json:"static"`
	StaticImmutable  List              `yaml:"static_immutable" json:"static_immutable"`
	StaticExtensions List              `yaml:"static_extensions" json:"static_extensions"`
	MaxBody          Size              `yaml:"max_body" json:"max_body"`
	BasicAuth        map[string]string `yaml:"basic_auth" json:"-"`
	AllowIPs         List              `yaml:"allow_ips" json:"allow_ips"`
	// Deny refuses a request path with 403 before the app is contacted: *.ext matches a suffix
	// and /path/* a subtree.
	Deny    List              `yaml:"deny" json:"deny"`
	Headers map[string]string `yaml:"headers" json:"headers"`
	Alerts  Alerts            `yaml:"alerts" json:"alerts"`
	Events  Events            `yaml:"events" json:"events"`
	// Auth puts an AuthCog sign-in in front of the app: exact addresses, *@domain patterns and a
	// bare * for any account; an empty list leaves the app open.
	Auth List `yaml:"auth" json:"auth"`
	// SessionTTL is how long a sign-in lasts, for the app gate and, on the host, the console.
	SessionTTL    Duration    `yaml:"session_ttl" json:"session_ttl"`
	AuthCog       AuthCogPath `yaml:"authcog" json:"authcog"`
	allowPrefixes []netip.Prefix
}

// DefaultAuthCogPath is the app URL a bare `authcog: true` captures, matching AuthCog's own
// default landing.
const DefaultAuthCogPath = "/authcog"

// AuthCogPath is the app-only login service: dboss runs the AuthCog round trip on the app's
// behalf at this path and hands the profile to the app once, so the app needs no AuthCog code of
// its own. `true` is DefaultAuthCogPath, a path sets it, `false` or empty turns it off.
type AuthCogPath string

// Enabled reports whether the app delegates login to dboss.
func (a AuthCogPath) Enabled() bool { return a != "" }

// UnmarshalYAML accepts true, false or a path.
func (a *AuthCogPath) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return &Error{Line: node.Line, Key: "authcog", Message: "must be true, false or a path"}
	}
	switch node.Tag {
	case "!!bool":
		*a = ""
		if node.Value == "true" {
			*a = DefaultAuthCogPath
		}
		return nil
	case "!!null":
		*a = ""
		return nil
	}
	*a = AuthCogPath(node.Value)
	return nil
}

// AuthAllows reports whether a signed-in email may reach the app.
func (w Web) AuthAllows(email string) bool {
	email = strings.ToLower(email)
	_, domain, found := strings.Cut(email, "@")
	if !found {
		return false
	}
	for _, entry := range w.Auth {
		entry = strings.ToLower(entry)
		if entry == "*" || entry == email || entry == "*@"+domain {
			return true
		}
	}
	return false
}

// Alerts are the request log checks behind the error-rate and slow notify events. ErrorRate is
// the percent of 5xx answers and SlowP95 the p95 latency that fires; 0 disables either check.
type Alerts struct {
	ErrorRate int      `yaml:"error_rate" json:"error_rate"`
	SlowP95   Duration `yaml:"slow_p95" json:"slow_p95"`
}

// The window the alert checks read and the requests it needs before a check runs, so a quiet
// app never pages.
const (
	AlertWindow      = 5 * time.Minute
	AlertMinRequests = 20
)

// Enabled reports whether any check is on.
func (a Alerts) Enabled() bool { return a.ErrorRate > 0 || a.SlowP95 > 0 }

// Events is the app's analytics. Every <ns>.json.log under the app's ./log is an event namespace
// dboss stores as Parquet instead of log rows. Retention bounds the raw events (0 stops ingest
// and keeps what is stored); Views and Funnels are saved queries by name, next to the ones saved
// from the console, and win over those on a name clash.
type Events struct {
	Retention Duration               `yaml:"retention" json:"retention"`
	Views     map[string]string      `yaml:"views" json:"views"`
	Funnels   map[string]EventFunnel `yaml:"funnels" json:"funnels"`
}

// EventFunnel is one saved funnel: by user, anon or tenant, a window from the first step, an
// optional breakdown and 2-10 steps, each a filter.
type EventFunnel struct {
	By        string      `yaml:"by" json:"by"`
	Window    Duration    `yaml:"window" json:"window"`
	Breakdown string      `yaml:"breakdown" json:"breakdown"`
	Steps     []EventStep `yaml:"steps" json:"steps"`
}

// EventStep is one funnel step.
type EventStep struct {
	Name   string `yaml:"name" json:"name"`
	Filter string `yaml:"filter" json:"filter"`
}

// Pubsub serves realtime channels on the app's own hosts under Path. An empty Path disables it.
// Secret is the bearer token HTTP publishers present; when empty dboss generates a per-app
// secret under state_dir. It never leaves the process as JSON, like the basic-auth hashes.
type Pubsub struct {
	Path           string `yaml:"path" json:"path"`
	Secret         string `yaml:"secret" json:"-"`
	Replay         int    `yaml:"replay" json:"replay"`
	MaxClients     int    `yaml:"max_clients" json:"max_clients"`
	MaxMessageSize Size   `yaml:"max_message_size" json:"max_message_size"`
	ClientEvents   bool   `yaml:"client_events" json:"client_events"`
	Test           bool   `yaml:"test" json:"test"`
}

// Enabled reports whether the app serves realtime channels.
func (p Pubsub) Enabled() bool { return p.Path != "" }

// defaultPubsub is the base every web process's hub starts from; the procfile entry overrides keys.
var defaultPubsub = Pubsub{Replay: 10, MaxClients: 500, MaxMessageSize: Size(64 << 10), ClientEvents: true}

// AllowPrefixes is allow_ips parsed at load time; empty means every client is allowed.
func (w Web) AllowPrefixes() []netip.Prefix { return w.allowPrefixes }
