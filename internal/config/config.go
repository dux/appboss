package config

import (
	"reflect"
	"slices"
	"strconv"
	"time"

	"dboss/internal/notify"
)

// Config is the root dboss.yaml: either a host that runs the apps found in Apps, or a single app (App set).
type Config struct {
	SourcePath string `yaml:"-" json:"-"`
	Dir        string `yaml:"-" json:"-"`
	App        *App   `yaml:"-" json:"-"`
	Apps       string `yaml:"apps" json:"apps"`
	// RuntimeDir holds everything dboss writes: StateDir, LogDir and Socket are derived from it.
	RuntimeDir string `yaml:"dir" json:"dir"`
	StateDir   string `yaml:"-" json:"state_dir"`
	LogDir     string `yaml:"-" json:"log_dir"`
	Socket     string `yaml:"-" json:"socket"`
	Ports      [2]int `yaml:"ports" json:"ports"`
	// ConsolePort is the loopback port the console is bound to; daemon.Build sets it.
	ConsolePort    int        `yaml:"-" json:"-"`
	LogLevel       string     `yaml:"log_level" json:"log_level"`
	AuditRetention Duration   `yaml:"audit_retention" json:"audit_retention"`
	MaintenanceAt  string     `yaml:"maintenance_at" json:"maintenance_at"`
	DiskAlert      int        `yaml:"disk_alert" json:"disk_alert"`
	AuthCogRealm   string     `yaml:"authcog_realm" json:"authcog_realm"`
	Tokens         Tokens     `yaml:"tokens" json:"tokens"`
	Proxy          Proxy      `yaml:"proxy" json:"proxy"`
	Management     Management `yaml:"management" json:"management"`
	Defaults       Defaults   `yaml:"defaults" json:"defaults"`
	Notify         Notify     `yaml:"notify" json:"notify"`
	Postgres       Postgres   `yaml:"postgres" json:"postgres"`
	// HostHooks and Pages are the host-level hooks: and pages: of the root file. They share their
	// key with the app file, so Parse copies them out of the embedded appFile.
	HostHooks map[string]Hook `yaml:"-" json:"-"`
	Pages     string          `yaml:"-" json:"pages"`
}

// Tokens are the credentials of the host. Github is outbound: a pull hook and a preview checkout
// use it for a private repository. Dboss is inbound: every hook ping and /metrics present it.
type Tokens struct {
	Github string `yaml:"github" json:"-"`
	Dboss  string `yaml:"dboss" json:"-"`
}

// Proxy has one listener per Listen address; every listener serves the same routing.
type Proxy struct {
	Listen List `yaml:"listen" json:"listen"`
	// Cloudflare accepts connections from Cloudflare's published ranges (and loopback) only and
	// reads the client address from CF-Connecting-IP.
	Cloudflare bool     `yaml:"cloudflare" json:"cloudflare"`
	TLS        ProxyTLS `yaml:"tls" json:"tls"`
	Timeout    Duration `yaml:"timeout" json:"timeout"`
}

// ProxyTLS terminates HTTPS on the box with ACME certificates, for a host that is reached
// directly instead of through Cloudflare. An empty listen disables it. Certificates are issued
// on demand for the hostnames of the current apps and the console, and cached under state_dir.
type ProxyTLS struct {
	Listen string `yaml:"listen" json:"listen"`
	Email  string `yaml:"email" json:"email"`
}

func (t ProxyTLS) Enabled() bool { return t.Listen != "" }

// Management is served by the proxy listener; any of the Host names selects the console.
type Management struct {
	Host   List `yaml:"host" json:"host"`
	Admins List `yaml:"admins" json:"admins"`
}

func (m Management) Enabled() bool { return len(m.Host) > 0 }

// ConsoleEnabled reports whether the console is served at all. A host serves it when it names a
// hostname; a dev session always gets it, on its loopback port, with no management block.
func (c Config) ConsoleEnabled() bool { return c.Management.Enabled() || c.Dev() }

// ConsoleURL is the console as something can reach it: the public management URL when one is
// configured, else the loopback port of a dev session once it is bound, else empty. Hook URLs
// are built from it.
func (c Config) ConsoleURL() string {
	if url := c.Management.PublicURL(); url != "" {
		return url
	}
	if !c.Dev() || c.ConsolePort == 0 {
		return ""
	}
	return "http://127.0.0.1:" + strconv.Itoa(c.ConsolePort)
}

// PublicURL is the address operators open and the base of the hook ping URLs: https on the first
// management host.
func (m Management) PublicURL() string {
	if len(m.Host) == 0 {
		return ""
	}
	return "https://" + m.Host[0]
}

// Notify posts runtime events to one operator webhook. An empty URL disables it; the payload
// shape follows the webhook's host.
type Notify struct {
	URL     string            `yaml:"url" json:"url"`
	Events  List              `yaml:"events" json:"events"`
	Headers map[string]string `yaml:"headers" json:"headers"`
}

func Default() Config {
	return Config{
		Apps:           "./apps",
		RuntimeDir:     "./.dboss",
		Ports:          [2]int{3100, 3990},
		LogLevel:       "info",
		AuditRetention: Duration(8760 * time.Hour),
		MaintenanceAt:  "04:10",
		DiskAlert:      90,
		AuthCogRealm:   "auth.authcog.com",
		Proxy:          Proxy{Listen: List{":80"}, Timeout: Duration(60 * time.Second)},
		Defaults:       Defaults{Process: Process{Health: "tcp", LivenessInterval: Duration(10 * time.Second), HealthTimeout: Duration(60 * time.Second), UnhealthyThreshold: 3, StopTimeout: Duration(20 * time.Second), StopSignal: "TERM", Restart: "on-failure", MaxRestarts: 5, LogRetention: Duration(336 * time.Hour), StdoutRetention: Duration(3 * time.Hour), TmpClean: Duration(7 * 24 * time.Hour), Env: map[string]string{}}, Web: Web{HealthEndpoint: "/.well-known/dboss/health", StaticImmutable: List{"/assets/"}, StaticExtensions: List{"css", "js", "mjs", "map", "json", "txt", "xml", "ico", "png", "jpg", "jpeg", "gif", "svg", "webp", "avif", "woff", "woff2", "ttf", "otf", "eot", "mp4", "webm", "mp3", "pdf", "wasm", "webmanifest"}, BasicAuth: map[string]string{}, Headers: map[string]string{}, Alerts: Alerts{ErrorRate: 10}, Events: Events{Retention: Duration(365 * 24 * time.Hour)}, SessionTTL: Duration(24 * time.Hour)}},
		Notify:         Notify{Events: List(slices.Clone(notify.Events)), Headers: map[string]string{}},
		Postgres:       Postgres{Enabled: true},
		Pages:          DefaultPages,
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
		{"dir", old.RuntimeDir != current.RuntimeDir},
		{"ports", old.Ports != current.Ports},
		{"log_level", old.LogLevel != current.LogLevel},
		{"audit_retention", old.AuditRetention != current.AuditRetention},
		{"maintenance_at", old.MaintenanceAt != current.MaintenanceAt},
		{"authcog_realm", old.AuthCogRealm != current.AuthCogRealm},
		{"proxy", !reflect.DeepEqual(old.Proxy, current.Proxy)},
		{"management", !reflect.DeepEqual(old.Management, current.Management)},
		{"notify", !reflect.DeepEqual(old.Notify, current.Notify)},
	} {
		if key.changed {
			keys = append(keys, key.name)
		}
	}
	return keys
}
