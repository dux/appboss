package config

import (
	"net/netip"
	"strings"
)

// Defaults holds every app-level key. Process keys can be overridden again per process, Web
// keys apply to the app as a whole. Both groups are flattened in YAML and JSON.
type Defaults struct {
	Process `yaml:",inline"`
	Web     `yaml:",inline"`
	Deploy  `yaml:",inline"`
}

type Process struct {
	IdleStop Duration `yaml:"idle_stop" json:"idle_stop"`
	// Health is resolved from the web process's procfile health path; it is not a settable key.
	Health             string            `yaml:"-" json:"-"`
	HealthInterval     Duration          `yaml:"health_interval" json:"health_interval"`
	LivenessInterval   Duration          `yaml:"liveness_interval" json:"liveness_interval"`
	HealthTimeout      Duration          `yaml:"health_timeout" json:"health_timeout"`
	UnhealthyThreshold int               `yaml:"unhealthy_threshold" json:"unhealthy_threshold"`
	StopTimeout        Duration          `yaml:"stop_timeout" json:"stop_timeout"`
	StopSignal         string            `yaml:"stop_signal" json:"stop_signal"`
	Restart            string            `yaml:"restart" json:"restart"`
	MaxRestarts        int               `yaml:"max_restarts" json:"max_restarts"`
	RestartReset       Duration          `yaml:"restart_reset" json:"restart_reset"`
	RestartBackoff     []any             `yaml:"restart_backoff" json:"restart_backoff"`
	LogMaxSize         Size              `yaml:"log_max_size" json:"log_max_size"`
	LogKeep            int               `yaml:"log_keep" json:"log_keep"`
	LogTailLines       int               `yaml:"log_tail_lines" json:"log_tail_lines"`
	LogRetention       Duration          `yaml:"log_retention" json:"log_retention"`
	StdoutRetention    Duration          `yaml:"stdout_retention" json:"stdout_retention"`
	TmpClean           Duration          `yaml:"tmp_clean" json:"tmp_clean"`
	Env                map[string]string `yaml:"env" json:"env"`
	Resources          string            `yaml:"resources" json:"resources"`
	MemoryMax          Size              `yaml:"memory_max" json:"memory_max"`
	CPUMax             int               `yaml:"cpu_max" json:"cpu_max"`
}

// Web drives the proxy in front of the app. BasicAuth never leaves the process as JSON so the
// hashes stay out of the console and `dboss status --json`.
type Web struct {
	HealthEndpoint   string            `yaml:"health_endpoint" json:"health_endpoint"`
	StaticImmutable  List              `yaml:"static_immutable" json:"static_immutable"`
	StaticExtensions List              `yaml:"static_extensions" json:"static_extensions"`
	MaxBody          Size              `yaml:"max_body" json:"max_body"`
	BasicAuth        map[string]string `yaml:"basic_auth" json:"-"`
	AllowIPs         List              `yaml:"allow_ips" json:"allow_ips"`
	Headers          map[string]string `yaml:"headers" json:"headers"`
	MaintenancePage  string            `yaml:"maintenance_page" json:"maintenance_page"`
	ErrorPagePath    string            `yaml:"error_page_path" json:"error_page_path"`
	Alerts           Alerts            `yaml:"alerts" json:"alerts"`
	Auth             Auth              `yaml:"auth" json:"auth"`
	AuthCog          AuthCog           `yaml:"authcog" json:"authcog"`
	allowPrefixes    []netip.Prefix
}

// AuthCog is the app-only login service: dboss runs the AuthCog round trip on the app's behalf
// and hands the profile to the app once, so the app needs no AuthCog code of its own. Login
// turns it on; Path is the app URL dboss captures (default /authcog, matching AuthCog's own
// default landing); Realm is the AuthCog subdomain (default auth, i.e. auth.authcog.com).
type AuthCog struct {
	Login bool   `yaml:"login" json:"login"`
	Path  string `yaml:"path" json:"path"`
	Realm string `yaml:"realm" json:"realm"`
}

// Enabled reports whether the app delegates login to dboss.
func (a AuthCog) Enabled() bool { return a.Login }

// RealmHost is the AuthCog host the login is sent to, built from the realm label.
func (a AuthCog) RealmHost() string {
	realm := a.Realm
	if realm == "" {
		realm = "auth"
	}
	return realm + ".authcog.com"
}

// Auth puts an AuthCog sign-in in front of the app. AllowEmails holds exact addresses,
// *@domain patterns and a bare * for any account; an empty list leaves the app open.
type Auth struct {
	AllowEmails List     `yaml:"allow_emails" json:"allow_emails"`
	SessionTTL  Duration `yaml:"session_ttl" json:"session_ttl"`
}

// Enabled reports whether visitors must sign in.
func (a Auth) Enabled() bool { return len(a.AllowEmails) > 0 }

// Allows reports whether a signed-in email may reach the app.
func (a Auth) Allows(email string) bool {
	email = strings.ToLower(email)
	_, domain, found := strings.Cut(email, "@")
	if !found {
		return false
	}
	for _, entry := range a.AllowEmails {
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
	Window      Duration `yaml:"window" json:"window"`
	MinRequests int      `yaml:"min_requests" json:"min_requests"`
	ErrorRate   int      `yaml:"error_rate" json:"error_rate"`
	SlowP95     Duration `yaml:"slow_p95" json:"slow_p95"`
}

// Enabled reports whether any check is on.
func (a Alerts) Enabled() bool { return a.ErrorRate > 0 || a.SlowP95 > 0 }

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
