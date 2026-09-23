package config

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"dboss/internal/notify"
)

// Config is the root dboss.yaml: either a host that runs the apps found in Apps, or a single app (App set).
type Config struct {
	SourcePath string     `yaml:"-" json:"-"`
	Dir        string     `yaml:"-" json:"-"`
	App        *App       `yaml:"-" json:"-"`
	Apps       string     `yaml:"apps" json:"apps"`
	StateDir   string     `yaml:"state_dir" json:"state_dir"`
	LogDir     string     `yaml:"log_dir" json:"log_dir"`
	Socket     string     `yaml:"socket" json:"socket"`
	Proxy      Proxy      `yaml:"proxy" json:"proxy"`
	Management Management `yaml:"management" json:"management"`
	Ports      Ports      `yaml:"ports" json:"ports"`
	Defaults   Defaults   `yaml:"defaults" json:"defaults"`
	Daemon     Daemon     `yaml:"daemon" json:"daemon"`
	Notify     Notify     `yaml:"notify" json:"notify"`
	Postgres   Postgres   `yaml:"postgres" json:"postgres"`
	// HostHooks is the host-level hooks: block of the root file. It shares the `hooks` key with an
	// app's hooks, so Parse copies it out of the embedded appFile rather than a yaml field here.
	HostHooks map[string]Hook `yaml:"-" json:"-"`
}

// Postgres is the host's PostgreSQL server. The console inspects it read-only and the daemon
// backs up the selected databases on a schedule. An empty DSN auto-detects a local server.
type Postgres struct {
	Enabled bool           `yaml:"enabled" json:"enabled"`
	DSN     string         `yaml:"dsn" json:"dsn,omitempty"`
	Backup  PostgresBackup `yaml:"backup" json:"backup"`
}

// PostgresBackup lists the databases that are dumped on the daily run, each with its rotation
// window. A key being present selects the database; manual backups from the console are kept
// outside the window.
type PostgresBackup struct {
	Databases map[string]DatabaseBackup `yaml:"databases" json:"databases"`
}

// DatabaseBackup overrides the backup policy for one database. Rotation is week or month; empty
// means week.
type DatabaseBackup struct {
	Rotation string `yaml:"rotation,omitempty" json:"rotation,omitempty"`
}

// Proxy has one listener per Listen address; every listener serves the same routing.
type Proxy struct {
	Listen          List `yaml:"listen" json:"listen"`
	TrustedCIDRs    List `yaml:"trusted_cidrs" json:"trusted_cidrs"`
	ClientIPHeaders List `yaml:"client_ip_headers" json:"client_ip_headers"`
	// CloudflareOnly refuses a request that does not carry Cloudflare's edge headers; it is a
	// convenience alternative to listing trusted_cidrs, not a replacement.
	CloudflareOnly bool     `yaml:"cloudflare_only" json:"cloudflare_only"`
	TLS            ProxyTLS `yaml:"tls" json:"tls"`
	Wake           Wake     `yaml:"wake" json:"wake"`
	Upstream       Upstream `yaml:"upstream" json:"upstream"`
}

// ProxyTLS terminates HTTPS on the box with ACME certificates, for a host that is reached
// directly instead of through Cloudflare. An empty listen disables it. Certificates are issued
// on demand for the hostnames of the current apps and the console, and cached under state_dir.
type ProxyTLS struct {
	Listen    string `yaml:"listen" json:"listen"`
	Email     string `yaml:"email" json:"email"`
	Directory string `yaml:"directory" json:"directory"`
	CacheDir  string `yaml:"cache_dir" json:"cache_dir"`
	Redirect  bool   `yaml:"redirect" json:"redirect"`
}

func (t ProxyTLS) Enabled() bool { return t.Listen != "" }

// Management is served by the proxy listener; any of the Host names selects the console.
type Management struct {
	Host    List              `yaml:"host" json:"host"`
	URL     string            `yaml:"url" json:"url"`
	Auth    ManagementAuth    `yaml:"auth" json:"auth"`
	Metrics ManagementMetrics `yaml:"metrics" json:"metrics"`
}

func (m Management) Enabled() bool { return len(m.Host) > 0 }

// ConsoleEnabled reports whether the console is served at all. A host serves it when it names a
// hostname; a dev session always gets it, on its loopback port, with no management block.
func (c Config) ConsoleEnabled() bool { return c.Management.Enabled() || c.Dev() }

// ConsoleURL is the console as something can reach it: the public management URL when one is
// configured, else the loopback port of a dev session, else empty. Hook URLs are built from it.
func (c Config) ConsoleURL() string {
	if url := c.Management.PublicURL(); url != "" {
		return url
	}
	if !c.Dev() {
		return ""
	}
	return "http://127.0.0.1:" + strconv.Itoa(c.Ports.Range[0])
}

// PublicURL is the address operators open and the base of the hook ping URLs. url wins when set;
// otherwise it defaults to https on the first management host, so host alone is enough.
func (m Management) PublicURL() string {
	if m.URL != "" {
		return strings.TrimSuffix(m.URL, "/")
	}
	if len(m.Host) == 0 {
		return ""
	}
	return "https://" + m.Host[0]
}

// ManagementMetrics exposes /healthz, /readyz and /metrics on the management host. A token, when
// set, is required as a bearer token for /metrics; /healthz and /readyz stay open.
type ManagementMetrics struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Token   string `yaml:"token" json:"-"`
}

type ManagementAuth struct {
	Realm       string   `yaml:"realm" json:"realm"`
	AdminEmails List     `yaml:"admin_emails" json:"admin_emails"`
	SessionTTL  Duration `yaml:"session_ttl" json:"session_ttl"`
}

type Wake struct {
	RetryAfter   int    `yaml:"retry_after" json:"retry_after"`
	StartingPage string `yaml:"starting_page" json:"starting_page"`
	CrashedPage  string `yaml:"crashed_page" json:"crashed_page"`
	UnknownPage  string `yaml:"unknown_page" json:"unknown_page"`
}

type Upstream struct {
	DialTimeout           Duration `yaml:"dial_timeout" json:"dial_timeout"`
	ResponseHeaderTimeout Duration `yaml:"response_header_timeout" json:"response_header_timeout"`
	IdleConnTimeout       Duration `yaml:"idle_conn_timeout" json:"idle_conn_timeout"`
	MaxIdleConnsPerApp    int      `yaml:"max_idle_conns_per_app" json:"max_idle_conns_per_app"`
}

type Ports struct {
	Range [2]int `yaml:"range" json:"range"`
}

type Daemon struct {
	IdleTick          Duration `yaml:"idle_tick" json:"idle_tick"`
	ResumeRunning     bool     `yaml:"resume_running" json:"resume_running"`
	PruneAt           string   `yaml:"prune_at" json:"prune_at"`
	VacuumAt          string   `yaml:"vacuum_at" json:"vacuum_at"`
	LogLevel          string   `yaml:"log_level" json:"log_level"`
	LogIngestInterval Duration `yaml:"log_ingest_interval" json:"log_ingest_interval"`
	LogFlush          Duration `yaml:"log_flush" json:"log_flush"`
	AuditRetention    Duration `yaml:"audit_retention" json:"audit_retention"`
}

// Notify posts runtime events to one operator webhook. An empty URL disables it.
type Notify struct {
	URL         string            `yaml:"url" json:"url"`
	Format      string            `yaml:"format" json:"format"`
	Events      List              `yaml:"events" json:"events"`
	MinInterval Duration          `yaml:"min_interval" json:"min_interval"`
	Headers     map[string]string `yaml:"headers" json:"headers"`
}

func Default() Config {
	return Config{
		Apps:     "./apps",
		StateDir: ".dboss/state", LogDir: ".dboss/log", Socket: ".dboss/dboss.sock",
		Proxy:      Proxy{Listen: List{":80"}, ClientIPHeaders: List{"CF-Connecting-IP", "X-Forwarded-For"}, TLS: ProxyTLS{Redirect: true}, Wake: Wake{RetryAfter: 5}, Upstream: Upstream{DialTimeout: Duration(2 * time.Second), ResponseHeaderTimeout: Duration(60 * time.Second), IdleConnTimeout: Duration(90 * time.Second), MaxIdleConnsPerApp: 32}},
		Management: Management{Auth: ManagementAuth{Realm: "auth.authcog.com", SessionTTL: Duration(24 * time.Hour)}, Metrics: ManagementMetrics{Enabled: true}},
		Ports:      Ports{Range: [2]int{3100, 3990}},
		Defaults:   Defaults{Process: Process{IdleStop: Duration(6 * time.Hour), Health: "tcp", HealthInterval: Duration(500 * time.Millisecond), LivenessInterval: Duration(10 * time.Second), HealthTimeout: Duration(60 * time.Second), UnhealthyThreshold: 3, StopTimeout: Duration(20 * time.Second), StopSignal: "TERM", Restart: "on-failure", MaxRestarts: 5, RestartReset: Duration(60 * time.Second), RestartBackoff: []any{"1s", 2.0, "60s"}, LogMaxSize: Size(10 << 20), LogKeep: 5, LogTailLines: 500, LogRetention: Duration(336 * time.Hour), StdoutRetention: Duration(3 * time.Hour), TmpClean: Duration(7 * 24 * time.Hour), Env: map[string]string{}, Resources: "auto"}, Web: Web{HealthEndpoint: "/.well-known/dboss/health", StaticImmutable: List{"/assets/"}, StaticExtensions: List{"css", "js", "mjs", "map", "json", "txt", "xml", "ico", "png", "jpg", "jpeg", "gif", "svg", "webp", "avif", "woff", "woff2", "ttf", "otf", "eot", "mp4", "webm", "mp3", "pdf", "wasm", "webmanifest"}, BasicAuth: map[string]string{}, Headers: map[string]string{}, Alerts: Alerts{Window: Duration(5 * time.Minute), MinRequests: 20, ErrorRate: 10}, Auth: Auth{SessionTTL: Duration(24 * time.Hour)}, AuthCog: AuthCog{Realm: "auth", Path: "/authcog"}}},
		Daemon:     Daemon{IdleTick: Duration(time.Minute), ResumeRunning: true, PruneAt: "04:10", VacuumAt: "04:30", LogLevel: "info", LogIngestInterval: Duration(5 * time.Second), LogFlush: Duration(time.Second), AuditRetention: Duration(8760 * time.Hour)},
		Notify:     Notify{Format: "generic", Events: List(slices.Clone(notify.Events)), MinInterval: Duration(5 * time.Minute), Headers: map[string]string{}},
		Postgres:   Postgres{Enabled: true, Backup: PostgresBackup{}},
	}
}

// RestartRequired lists the host keys that differ between the config a session started with and
// a freshly loaded one. Everything else applies live through rescan.
func RestartRequired(old, current Config) []string {
	var keys []string
	for _, key := range []struct {
		name    string
		changed bool
	}{
		{"apps", old.Apps != current.Apps},
		{"state_dir", old.StateDir != current.StateDir},
		{"log_dir", old.LogDir != current.LogDir},
		{"socket", old.Socket != current.Socket},
		{"proxy", !reflect.DeepEqual(old.Proxy, current.Proxy)},
		{"management", !reflect.DeepEqual(old.Management, current.Management)},
		{"ports", old.Ports != current.Ports},
		{"daemon", old.Daemon != current.Daemon},
		{"notify", !reflect.DeepEqual(old.Notify, current.Notify)},
	} {
		if key.changed {
			keys = append(keys, key.name)
		}
	}
	return keys
}
