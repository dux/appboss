package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"dboss/internal/schedule"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	v, err := time.ParseDuration(node.Value)
	if err != nil {
		return &Error{Line: node.Line, Message: fmt.Sprintf("invalid duration %q", node.Value), Hint: "durations look like 500ms, 30s, 20m, 6h or 72h"}
	}
	*d = Duration(v)
	return nil
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		var nanos int64
		if err := json.Unmarshal(data, &nanos); err != nil {
			return err
		}
		*d = Duration(nanos)
		return nil
	}
	value, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("invalid duration %q", text)
	}
	*d = Duration(value)
	return nil
}

func (d Duration) MarshalYAML() (any, error)    { return time.Duration(d).String(), nil }
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }
func (d Duration) Value() time.Duration         { return time.Duration(d) }

type Size int64

func (s *Size) UnmarshalYAML(node *yaml.Node) error {
	v, err := ParseSize(node.Value)
	if err != nil {
		return &Error{Line: node.Line, Message: fmt.Sprintf("invalid size %q", node.Value), Hint: "sizes look like 1024, 512k, 10m or 1g"}
	}
	*s = Size(v)
	return nil
}

func ParseSize(value string) (int64, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "0" {
		return 0, nil
	}
	multiplier := int64(1)
	if len(value) > 0 {
		switch value[len(value)-1] {
		case 'k':
			multiplier = 1 << 10
		case 'm':
			multiplier = 1 << 20
		case 'g':
			multiplier = 1 << 30
		default:
			if value[len(value)-1] < '0' || value[len(value)-1] > '9' {
				return 0, fmt.Errorf("invalid size %q (use bytes, k, m, or g)", value)
			}
			value += "b"
		}
	}
	if value == "" {
		return 0, fmt.Errorf("invalid size %q (use bytes, k, m, or g)", value)
	}
	n, err := strconv.ParseInt(value[:len(value)-1], 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	return n * multiplier, nil
}

func (s Size) MarshalYAML() (any, error) { return s.String(), nil }
func (s Size) String() string {
	value := int64(s)
	if value == 0 {
		return "0"
	}
	for _, unit := range []struct {
		suffix string
		bytes  int64
	}{{"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}} {
		if value%unit.bytes == 0 {
			return fmt.Sprintf("%d%s", value/unit.bytes, unit.suffix)
		}
	}
	return strconv.FormatInt(value, 10)
}

// List is a []string that also accepts a single scalar in YAML, so `hosts: myapp.com` and
// `hosts: [myapp.com]` mean the same thing. It marshals as a sequence.
type List []string

func (l *List) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			*l = nil
			return nil
		}
		*l = List{node.Value}
		return nil
	case yaml.SequenceNode:
		var items []string
		if err := node.Decode(&items); err != nil {
			return err
		}
		*l = List(items)
		return nil
	}
	return &Error{Line: node.Line, Message: "must be a value or a list of values"}
}

// FileName and LocalFileName are the two config file names looked up in a folder.
// The local file is server-only and, when present, replaces the committed one entirely.
const (
	FileName      = "dboss.yaml"
	LocalFileName = "dboss.local.yaml"
)

// FindInDir returns the config file to use for dir: dboss.local.yaml when it exists, else dboss.yaml.
func FindInDir(dir string) (string, error) {
	for _, name := range []string{LocalFileName, FileName} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no %s in %s", FileName, dir)
}

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
	S3         S3         `yaml:"s3" json:"s3"`
}

// Postgres is the host's PostgreSQL server. The console inspects it read-only and the daemon
// backs up the selected databases on a schedule. An empty DSN auto-detects a local server.
type Postgres struct {
	Enabled bool           `yaml:"enabled" json:"enabled"`
	DSN     string         `yaml:"dsn" json:"dsn,omitempty"`
	Backup  PostgresBackup `yaml:"backup" json:"backup"`
}

// PostgresBackup is the host-wide backup policy. Databases maps a database name to its
// destination overrides; a key being present selects the database.
type PostgresBackup struct {
	Dir       string            `yaml:"dir" json:"dir"`
	S3        bool              `yaml:"s3" json:"s3"`
	Every     Duration          `yaml:"every" json:"every"`
	Timeout   Duration          `yaml:"timeout" json:"timeout"`
	Globals   bool              `yaml:"globals" json:"globals"`
	Keep      PostgresKeep      `yaml:"keep" json:"keep"`
	Databases map[string]Target `yaml:"databases" json:"databases"`
}

// PostgresKeep is the grandfather-father-son retention policy: the newest N dumps are kept in
// each bucket and the rest are pruned. Zero disables that bucket.
type PostgresKeep struct {
	Hourly  int `yaml:"hourly" json:"hourly"`
	Daily   int `yaml:"daily" json:"daily"`
	Weekly  int `yaml:"weekly" json:"weekly"`
	Monthly int `yaml:"monthly" json:"monthly"`
}

// Target overrides where one database's dumps go. A nil field follows the backup-level default.
type Target struct {
	Local *bool `yaml:"local" json:"local,omitempty"`
	S3    *bool `yaml:"s3" json:"s3,omitempty"`
}

// S3 is the optional object storage used for off-host backup copies. Empty endpoint or bucket
// disables it.
type S3 struct {
	Endpoint  string `yaml:"endpoint" json:"endpoint"`
	Region    string `yaml:"region" json:"region"`
	Bucket    string `yaml:"bucket" json:"bucket"`
	Prefix    string `yaml:"prefix" json:"prefix"`
	AccessKey string `yaml:"access_key" json:"access_key,omitempty"`
	SecretKey string `yaml:"secret_key" json:"-"`
	PathStyle bool   `yaml:"path_style" json:"path_style"`
	SSE       string `yaml:"sse" json:"sse"`
}

// Proxy has one listener per Listen address; every listener serves the same routing.
type Proxy struct {
	Listen          List `yaml:"listen" json:"listen"`
	TrustedCIDRs    List `yaml:"trusted_cidrs" json:"trusted_cidrs"`
	ClientIPHeaders List `yaml:"client_ip_headers" json:"client_ip_headers"`
	// CloudflareOnly refuses a request that does not carry Cloudflare's edge headers; it is a
	// convenience alternative to listing trusted_cidrs, not a replacement.
	CloudflareOnly bool     `yaml:"cloudflare_only" json:"cloudflare_only"`
	Wake           Wake     `yaml:"wake" json:"wake"`
	Upstream       Upstream `yaml:"upstream" json:"upstream"`
}

// Management is served by the proxy listener; any of the Host names selects the console.
type Management struct {
	Host    List              `yaml:"host" json:"host"`
	URL     string            `yaml:"url" json:"url"`
	Auth    ManagementAuth    `yaml:"auth" json:"auth"`
	Metrics ManagementMetrics `yaml:"metrics" json:"metrics"`
}

func (m Management) Enabled() bool { return len(m.Host) > 0 }

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

// Defaults holds every app-level key. Process keys can be overridden again per process, Web
// keys apply to the app as a whole. Both groups are flattened in YAML and JSON.
type Defaults struct {
	Process `yaml:",inline"`
	Web     `yaml:",inline"`
}

type Process struct {
	IdleStop           Duration          `yaml:"idle_stop" json:"idle_stop"`
	Health             string            `yaml:"health" json:"health"`
	HealthInterval     Duration          `yaml:"health_interval" json:"health_interval"`
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
	LogFlush           Duration          `yaml:"log_flush" json:"log_flush"`
	Shell              bool              `yaml:"shell" json:"shell"`
	Env                map[string]string `yaml:"env" json:"env"`
	Resources          string            `yaml:"resources" json:"resources"`
	MemoryMax          Size              `yaml:"memory_max" json:"memory_max"`
	CPUMax             int               `yaml:"cpu_max" json:"cpu_max"`
}

// Web drives the proxy in front of the app. BasicAuth never leaves the process as JSON so the
// hashes stay out of the console and `dboss status --json`.
type Web struct {
	HealthEndpoint  string            `yaml:"health_endpoint" json:"health_endpoint"`
	Static          string            `yaml:"static" json:"static"`
	StaticImmutable List              `yaml:"static_immutable" json:"static_immutable"`
	MaxBody         Size              `yaml:"max_body" json:"max_body"`
	BasicAuth       map[string]string `yaml:"basic_auth" json:"-"`
	AllowIPs        List              `yaml:"allow_ips" json:"allow_ips"`
	Headers         map[string]string `yaml:"headers" json:"headers"`
	MaintenancePage string            `yaml:"maintenance_page" json:"maintenance_page"`
	Pubsub          Pubsub            `yaml:"pubsub" json:"pubsub"`
	allowPrefixes   []netip.Prefix
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

// AllowPrefixes is allow_ips parsed at load time; empty means every client is allowed.
func (w Web) AllowPrefixes() []netip.Prefix { return w.allowPrefixes }

type Daemon struct {
	IdleTick          Duration `yaml:"idle_tick" json:"idle_tick"`
	ResumeRunning     bool     `yaml:"resume_running" json:"resume_running"`
	PruneAt           string   `yaml:"prune_at" json:"prune_at"`
	VacuumAt          string   `yaml:"vacuum_at" json:"vacuum_at"`
	LogLevel          string   `yaml:"log_level" json:"log_level"`
	LogIngestInterval Duration `yaml:"log_ingest_interval" json:"log_ingest_interval"`
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
		Proxy:      Proxy{Listen: List{":80"}, ClientIPHeaders: List{"CF-Connecting-IP", "X-Forwarded-For"}, Wake: Wake{RetryAfter: 5, StartingPage: "web/starting.html", CrashedPage: "web/crashed.html", UnknownPage: "web/404.html"}, Upstream: Upstream{DialTimeout: Duration(2 * time.Second), ResponseHeaderTimeout: Duration(60 * time.Second), IdleConnTimeout: Duration(90 * time.Second), MaxIdleConnsPerApp: 32}},
		Management: Management{Auth: ManagementAuth{Realm: "auth.authcog.com", SessionTTL: Duration(24 * time.Hour)}, Metrics: ManagementMetrics{Enabled: true}},
		Ports:      Ports{Range: [2]int{3100, 3990}},
		Defaults:   Defaults{Process: Process{IdleStop: Duration(6 * time.Hour), Health: "tcp", HealthInterval: Duration(500 * time.Millisecond), HealthTimeout: Duration(60 * time.Second), UnhealthyThreshold: 3, StopTimeout: Duration(20 * time.Second), StopSignal: "TERM", Restart: "on-failure", MaxRestarts: 5, RestartReset: Duration(60 * time.Second), RestartBackoff: []any{"1s", 2.0, "60s"}, LogMaxSize: Size(10 << 20), LogKeep: 5, LogTailLines: 500, LogRetention: Duration(336 * time.Hour), StdoutRetention: Duration(3 * time.Hour), LogFlush: Duration(time.Second), Env: map[string]string{}, Resources: "auto"}, Web: Web{HealthEndpoint: "/.well-known/dboss/health", StaticImmutable: List{"/assets/"}, BasicAuth: map[string]string{}, Headers: map[string]string{}, Pubsub: Pubsub{Replay: 10, MaxClients: 500, MaxMessageSize: Size(64 << 10), ClientEvents: true}}},
		Daemon:     Daemon{IdleTick: Duration(time.Minute), ResumeRunning: true, PruneAt: "04:10", VacuumAt: "04:30", LogLevel: "info", LogIngestInterval: Duration(5 * time.Second), AuditRetention: Duration(8760 * time.Hour)},
		Notify:     Notify{Format: "generic", Events: List{"crash", "restart-loop", "health-timeout", "wake-failed", "hook-failed", "deploy", "config-changed", "backup-failed"}, MinInterval: Duration(5 * time.Minute), Headers: map[string]string{}},
		Postgres:   Postgres{Enabled: true, Backup: PostgresBackup{Dir: ".dboss/pg-backups", S3: true, Every: Duration(6 * time.Hour), Timeout: Duration(time.Hour), Globals: true, Keep: PostgresKeep{Hourly: 24, Daily: 7, Weekly: 8, Monthly: 6}}},
		S3:         S3{Region: "auto"},
	}
}

// file is the full dboss.yaml schema: host keys plus app keys. Which role the file plays
// is decided after decoding from whether procfile or apps is present.
type file struct {
	Config  `yaml:",inline"`
	appFile `yaml:",inline"`
}

var hostKeys = []string{"apps", "state_dir", "log_dir", "socket", "proxy", "management", "ports", "defaults", "daemon", "notify", "postgres", "s3"}

// decode parses one document into raw and reports every top-level key present in it. The node
// tree is kept so every error can be pointed at a line and a key.
func decode(data []byte, path string, raw *file) (map[string]bool, *yaml.Node, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, located(err, path, nil)
	}
	if err := checkKeys(&root, reflect.TypeOf(file{}), ""); err != nil {
		return nil, nil, located(err, path, &root)
	}
	if expandEnv(&root, "") && len(root.Content) > 0 {
		if err := root.Content[0].Decode(raw); err != nil && !errors.Is(err, io.EOF) {
			return nil, nil, located(err, path, &root)
		}
	} else {
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(raw); err != nil && !errors.Is(err, io.EOF) {
			return nil, nil, located(err, path, &root)
		}
	}
	keys := map[string]bool{}
	if len(root.Content) > 0 && root.Content[0].Kind == yaml.MappingNode {
		for i := 0; i+1 < len(root.Content[0].Content); i += 2 {
			keys[root.Content[0].Content[i].Value] = true
		}
	}
	return keys, &root, nil
}

// envRef matches $NAME in a config value. Only all-uppercase names are eligible, so a bcrypt
// hash ($2a$10$...), a shell positional ($1) and lowercase shell vars are not touched.
var envRef = regexp.MustCompile(`\$[A-Z_][A-Z0-9_]*`)

// expandEnv replaces $NAME in every string value with the matching process environment
// variable, leaving the text as written when NAME is unset. procfile values and
// cron.*.command are runtime shell lines, so they are skipped. It reports whether any value
// changed, which tells decode to read the mutated tree instead of the original bytes.
func expandEnv(node *yaml.Node, path string) bool {
	changed := false
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			changed = expandEnv(child, path) || changed
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			child := path + key.Value + "."
			if strings.HasPrefix(child, "procfile.") || strings.HasPrefix(child, "cron.") && strings.HasSuffix(child, ".command.") {
				continue
			}
			changed = expandEnv(value, child) || changed
		}
	case yaml.ScalarNode:
		if node.Tag != "!!str" || !strings.Contains(node.Value, "$") {
			return false
		}
		value := envRef.ReplaceAllStringFunc(node.Value, func(match string) string {
			if env, ok := os.LookupEnv(match[1:]); ok {
				return env
			}
			return match
		})
		if value == node.Value {
			return false
		}
		node.Value, node.Tag, node.Style = value, "", 0
		return true
	}
	return changed
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(data, path)
}

// Parse builds the root config from data as if it had been read from path, so a document can be
// validated before it is written to disk. Relative paths resolve against path's directory.
func Parse(data []byte, path string) (Config, error) {
	raw := file{Config: Default()}
	keys, root, err := decode(data, path, &raw)
	if err != nil {
		return Config{}, err
	}
	cfg := raw.Config
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	cfg.SourcePath = absolutePath
	cfg.Dir = filepath.Dir(absolutePath)
	hasApp := keys["procfile"]
	if hasApp && keys["apps"] {
		return Config{}, located(&Error{Message: "a file is either an app (procfile) or a host (apps), not both", Hint: "move the host keys to the root dboss.yaml or drop apps"}, path, root)
	}
	cfg.Apps = resolvePath(cfg.Dir, cfg.Apps)
	if hasApp {
		// Single mode: the root file is the app, so the host apps directory does not apply.
		cfg.Apps = ""
	}
	cfg.StateDir = resolvePath(cfg.Dir, cfg.StateDir)
	cfg.LogDir = resolvePath(cfg.Dir, cfg.LogDir)
	cfg.Socket = resolvePath(cfg.Dir, cfg.Socket)
	cfg.Postgres.Backup.Dir = resolvePath(cfg.Dir, cfg.Postgres.Backup.Dir)
	if err := cfg.validate(hasApp); err != nil {
		return Config{}, located(err, path, root)
	}
	if hasApp {
		app, err := buildApp(raw.appFile, cfg.Defaults)
		if err != nil {
			return Config{}, located(err, path, root)
		}
		cfg.App = &app
	}
	return cfg, nil
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
	} {
		if key.changed {
			keys = append(keys, key.name)
		}
	}
	return keys
}

func (c Config) Validate() error { return c.validate(c.App != nil) }

func (c Config) validate(hasApp bool) error {
	if c.Apps == "" && !hasApp {
		return &Error{Message: "apps directory or procfile is required"}
	}
	if c.StateDir == "" || c.LogDir == "" || c.Socket == "" {
		return &Error{Message: "state_dir, log_dir, and socket are required"}
	}
	if err := validateManagement(c.Management, len(c.Proxy.Listen) > 0); err != nil {
		return scoped(err, "management")
	}
	for _, cidr := range c.Proxy.TrustedCIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return &Error{Key: "proxy.trusted_cidrs", Message: fmt.Sprintf("invalid entry %q", cidr), Hint: "entries are CIDRs like 173.245.48.0/20 or 2400:cb00::/32"}
		}
	}
	if c.Ports.Range[0] < 1 || c.Ports.Range[1] > 65535 || c.Ports.Range[0] > c.Ports.Range[1] {
		return &Error{Key: "ports.range", Message: fmt.Sprintf("invalid range %v", c.Ports.Range), Hint: "give [first, last] between 1 and 65535, e.g. [3100, 3990]"}
	}
	if c.Proxy.Wake.RetryAfter < 1 {
		return keyErr("proxy.wake.retry_after", "must be positive")
	}
	listeners := map[string]bool{}
	for _, address := range c.Proxy.Listen {
		_, portValue, err := net.SplitHostPort(address)
		if err != nil {
			return &Error{Key: "proxy.listen", Message: fmt.Sprintf("invalid address %q", address), Hint: "use host:port such as \":80\" or 127.0.0.1:8080"}
		}
		port, err := strconv.Atoi(portValue)
		if err != nil || port < 1 || port > 65535 {
			return keyErr("proxy.listen", "invalid port %q", portValue)
		}
		if port >= c.Ports.Range[0] && port <= c.Ports.Range[1] {
			return keyErr("proxy.listen", "port %d overlaps ports.range", port)
		}
		if listeners[address] {
			return keyErr("proxy.listen", "duplicate entry %q", address)
		}
		listeners[address] = true
	}
	if err := validateDefaults(c.Defaults); err != nil {
		return scoped(err, "defaults")
	}
	if _, err := time.Parse("15:04", c.Daemon.PruneAt); err != nil {
		return &Error{Key: "daemon.prune_at", Message: fmt.Sprintf("invalid time %q", c.Daemon.PruneAt), Hint: "use 24h clock HH:MM, e.g. \"04:10\""}
	}
	if c.Daemon.VacuumAt != "" {
		if _, err := time.Parse("15:04", c.Daemon.VacuumAt); err != nil {
			return &Error{Key: "daemon.vacuum_at", Message: fmt.Sprintf("invalid time %q", c.Daemon.VacuumAt), Hint: "use 24h clock HH:MM, e.g. \"04:30\"; leave empty to disable"}
		}
	}
	if c.Daemon.IdleTick <= 0 {
		return keyErr("daemon.idle_tick", "must be positive")
	}
	if c.Daemon.LogIngestInterval <= 0 {
		return keyErr("daemon.log_ingest_interval", "must be positive")
	}
	if c.Daemon.AuditRetention < 0 {
		return keyErr("daemon.audit_retention", "cannot be negative")
	}
	if c.Daemon.LogLevel != "debug" && c.Daemon.LogLevel != "info" && c.Daemon.LogLevel != "warn" && c.Daemon.LogLevel != "error" {
		return keyErr("daemon.log_level", "must be debug, info, warn or error, not %q", c.Daemon.LogLevel)
	}
	if c.Proxy.Upstream.DialTimeout <= 0 || c.Proxy.Upstream.ResponseHeaderTimeout <= 0 || c.Proxy.Upstream.IdleConnTimeout <= 0 || c.Proxy.Upstream.MaxIdleConnsPerApp <= 0 {
		return keyErr("proxy.upstream", "timeouts and max_idle_conns_per_app must be positive")
	}
	if err := validateNotify(c.Notify); err != nil {
		return scoped(err, "notify")
	}
	if err := validateS3(c.S3); err != nil {
		return scoped(err, "s3")
	}
	if err := validatePostgres(c.Postgres, c.S3); err != nil {
		return scoped(err, "postgres")
	}
	return nil
}

// Destinations reports where a database's dumps go, applying its per-database override to the
// backup-level defaults. A database that is not in Databases still gets the defaults, so the
// caller decides whether it is selected by checking the map.
func (b PostgresBackup) Destinations(name string) (local, s3 bool) {
	local, s3 = b.Dir != "", b.S3
	if target, ok := b.Databases[name]; ok {
		if target.Local != nil {
			local = *target.Local
		}
		if target.S3 != nil {
			s3 = *target.S3
		}
	}
	return local, s3
}

// Selected lists the databases configured for backup.
func (b PostgresBackup) Selected() []string {
	names := make([]string, 0, len(b.Databases))
	for name := range b.Databases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var databaseName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}$`)

func validatePostgres(p Postgres, s3 S3) error {
	if !p.Enabled {
		return nil
	}
	for key, value := range map[string]Duration{"every": p.Backup.Every, "timeout": p.Backup.Timeout} {
		if value < 0 {
			return keyErr("backup."+key, "cannot be negative")
		}
	}
	for key, value := range map[string]int{"hourly": p.Backup.Keep.Hourly, "daily": p.Backup.Keep.Daily, "weekly": p.Backup.Keep.Weekly, "monthly": p.Backup.Keep.Monthly} {
		if value < 0 {
			return keyErr("backup.keep."+key, "cannot be negative")
		}
	}
	for _, name := range p.Backup.Selected() {
		if !databaseName.MatchString(name) {
			return &Error{Key: "backup.databases", Message: fmt.Sprintf("invalid database name %q", name), Hint: "names match [A-Za-z_][A-Za-z0-9_$]*, e.g. myapp_production"}
		}
		local, wants3 := p.Backup.Destinations(name)
		if !local && !wants3 {
			return keyErr("backup.databases", "database %q has neither a local nor an s3 destination", name)
		}
		if wants3 && !s3.Configured() {
			return &Error{Key: "backup.databases", Message: fmt.Sprintf("database %q targets s3 but s3 is not configured", name), Hint: "set s3.endpoint, s3.bucket and credentials, or set local: true"}
		}
	}
	return nil
}

// Configured reports whether the S3 block has enough to store objects.
func (s S3) Configured() bool {
	return s.Endpoint != "" && s.Bucket != "" && s.AccessKey != "" && s.SecretKey != ""
}

func validateS3(s S3) error {
	if s.Endpoint == "" && s.Bucket == "" {
		return nil
	}
	if s.Endpoint == "" || s.Bucket == "" {
		return &Error{Key: "endpoint", Message: "endpoint and bucket must be set together", Hint: "set both, or clear both to disable s3"}
	}
	if parsed, err := url.Parse(s.Endpoint); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return &Error{Key: "endpoint", Message: fmt.Sprintf("invalid URL %q", s.Endpoint), Hint: "use the full endpoint, e.g. https://<account>.r2.cloudflarestorage.com"}
	}
	if s.Region == "" {
		return keyErr("region", "must not be empty; use auto for R2 or the bucket's region")
	}
	if s.AccessKey == "" || s.SecretKey == "" {
		return keyErr("access_key", "access_key and secret_key are required when s3 is configured")
	}
	return nil
}

var notifyFormats = map[string]bool{"generic": true, "slack": true, "discord": true, "ntfy": true}
var notifyEvents = map[string]bool{"crash": true, "restart-loop": true, "health-timeout": true, "wake-failed": true, "hook-failed": true, "deploy": true, "config-changed": true, "backup-failed": true}

func validateNotify(n Notify) error {
	if !notifyFormats[n.Format] {
		return keyErr("format", "must be generic, slack, discord or ntfy, not %q", n.Format)
	}
	for _, event := range n.Events {
		if !notifyEvents[event] {
			return keyErr("events", "unknown event %q", event)
		}
	}
	if n.MinInterval < 0 {
		return keyErr("min_interval", "cannot be negative")
	}
	if n.URL != "" {
		parsed, err := url.Parse(n.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return &Error{Key: "url", Message: fmt.Sprintf("invalid URL %q", n.URL), Hint: "use the webhook address, e.g. https://hooks.slack.com/services/..."}
		}
	}
	for name := range n.Headers {
		if name == "" || strings.ContainsAny(name, ": \t") {
			return keyErr("headers", "invalid header name %q", name)
		}
	}
	return nil
}

func validateManagement(management Management, proxyEnabled bool) error {
	if !management.Enabled() {
		if len(management.Auth.AdminEmails) > 0 {
			return keyErr("host", "is required when management is configured")
		}
		return nil
	}
	if !proxyEnabled {
		return keyErr("host", "needs proxy.listen because the console is served by the proxy listener")
	}
	hosts := map[string]bool{}
	for _, host := range management.Host {
		if !validHostname(host) {
			return keyErr("host", "invalid hostname %q", host)
		}
		if hosts[strings.ToLower(host)] {
			return keyErr("host", "duplicate entry %q", host)
		}
		hosts[strings.ToLower(host)] = true
	}
	if management.URL != "" {
		parsed, err := url.Parse(management.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return &Error{Key: "url", Message: fmt.Sprintf("invalid URL %q", management.URL), Hint: "use the address operators open, e.g. https://dboss.example.com"}
		}
		if !hosts[strings.ToLower(parsed.Hostname())] {
			return keyErr("url", "host %q is not one of management.host", parsed.Hostname())
		}
	}
	if !validHostname(management.Auth.Realm) {
		return keyErr("auth.realm", "invalid hostname %q", management.Auth.Realm)
	}
	if management.Auth.SessionTTL <= 0 {
		return keyErr("auth.session_ttl", "must be positive")
	}
	if len(management.Auth.AdminEmails) == 0 {
		return keyErr("auth.admin_emails", "must contain at least one email")
	}
	emails := map[string]bool{}
	for _, email := range management.Auth.AdminEmails {
		address, err := mail.ParseAddress(email)
		if err != nil || !strings.EqualFold(address.Address, email) {
			return keyErr("auth.admin_emails", "invalid entry %q", email)
		}
		normalized := strings.ToLower(address.Address)
		if emails[normalized] {
			return keyErr("auth.admin_emails", "duplicate entry %q", email)
		}
		emails[normalized] = true
	}
	return nil
}

func validHostname(value string) bool {
	if value == "" || len(value) > 253 || strings.ContainsAny(value, "/: ") {
		return false
	}
	if net.ParseIP(value) != nil {
		return true
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if !(character == '-' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9') {
				return false
			}
		}
	}
	return true
}

func resolvePath(baseDir, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(baseDir, path))
}

func validateDefaults(d Defaults) error {
	if err := validateProcess(d.Process); err != nil {
		return err
	}
	return validateWeb(d.Web)
}

func validateProcess(d Process) error {
	if d.Health != "tcp" && !strings.HasPrefix(d.Health, "http:/") {
		return &Error{Key: "health", Message: fmt.Sprintf("must be tcp or http:/path, not %q", d.Health), Hint: "e.g. health: http:/up"}
	}
	if d.Restart != "on-failure" && d.Restart != "always" && d.Restart != "never" {
		return keyErr("restart", "must be on-failure, always or never, not %q", d.Restart)
	}
	validSignals := map[string]bool{"TERM": true, "INT": true, "QUIT": true, "USR1": true, "USR2": true}
	if !validSignals[d.StopSignal] {
		return keyErr("stop_signal", "must be TERM, INT, QUIT, USR1 or USR2, not %q", d.StopSignal)
	}
	for key, value := range map[string]int{"max_restarts": d.MaxRestarts, "log_keep": d.LogKeep, "log_tail_lines": d.LogTailLines, "unhealthy_threshold": d.UnhealthyThreshold} {
		if value < 0 {
			return keyErr(key, "cannot be negative")
		}
	}
	for key, value := range map[string]Duration{"health_interval": d.HealthInterval, "health_timeout": d.HealthTimeout, "restart_reset": d.RestartReset, "log_flush": d.LogFlush} {
		if value <= 0 {
			return keyErr(key, "must be positive")
		}
	}
	for key, value := range map[string]Duration{"stop_timeout": d.StopTimeout, "idle_stop": d.IdleStop, "log_retention": d.LogRetention, "stdout_retention": d.StdoutRetention} {
		if value < 0 {
			return keyErr(key, "cannot be negative")
		}
	}
	if d.Resources != "auto" && d.Resources != "procgroup" && d.Resources != "cgroup" {
		return keyErr("resources", "must be auto, procgroup or cgroup, not %q", d.Resources)
	}
	backoff := &Error{Key: "restart_backoff", Hint: "e.g. restart_backoff: [1s, 2.0, 60s]"}
	if len(d.RestartBackoff) != 3 {
		backoff.Message = "must contain first delay, multiplier, and cap"
		return backoff
	}
	first, err := time.ParseDuration(fmt.Sprint(d.RestartBackoff[0]))
	if err != nil || first <= 0 {
		backoff.Message = "first delay must be a positive duration"
		return backoff
	}
	multiplier, err := strconv.ParseFloat(fmt.Sprint(d.RestartBackoff[1]), 64)
	if err != nil || multiplier < 1 {
		backoff.Message = "multiplier must be at least 1"
		return backoff
	}
	maximum, err := time.ParseDuration(fmt.Sprint(d.RestartBackoff[2]))
	if err != nil || maximum < first {
		backoff.Message = "cap must be a duration of at least the first delay"
		return backoff
	}
	return nil
}

func validateWeb(w Web) error {
	if w.HealthEndpoint != "" && !strings.HasPrefix(w.HealthEndpoint, "/") {
		return keyErr("health_endpoint", "must start with /")
	}
	if _, err := parsePrefixes(w.AllowIPs); err != nil {
		return err
	}
	for user, hash := range w.BasicAuth {
		if user == "" || strings.ContainsAny(user, ": ") {
			return keyErr("basic_auth", "invalid user %q", user)
		}
		if _, err := bcrypt.Cost([]byte(hash)); err != nil {
			return &Error{Key: "basic_auth." + user, Message: "must be a bcrypt hash", Hint: "run `dboss password` to print one"}
		}
	}
	for name := range w.Headers {
		if name == "" || strings.ContainsAny(name, ": \t") {
			return keyErr("headers", "invalid header name %q", name)
		}
	}
	for _, prefix := range w.StaticImmutable {
		if !strings.HasPrefix(prefix, "/") {
			return keyErr("static_immutable", "%q must start with /", prefix)
		}
	}
	return validatePubsub(w.Pubsub)
}

func validatePubsub(p Pubsub) error {
	if p.Path == "" {
		return nil
	}
	invalid := !strings.HasPrefix(p.Path, "/") || p.Path == "/" || strings.HasSuffix(p.Path, "/") || strings.Contains(p.Path, "//")
	for _, character := range p.Path {
		if !(character == '/' || character == '-' || character == '_' || character == '.' || character == '~' ||
			character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9') {
			invalid = true
		}
	}
	if invalid {
		return &Error{Key: "pubsub.path", Message: fmt.Sprintf("invalid path %q", p.Path), Hint: "use a URL prefix such as /socketio (letters, digits, - _ . ~ and / only)"}
	}
	for key, value := range map[string]int{"replay": p.Replay, "max_clients": p.MaxClients} {
		if value < 0 {
			return keyErr("pubsub."+key, "cannot be negative")
		}
	}
	if p.MaxMessageSize < 0 {
		return keyErr("pubsub.max_message_size", "cannot be negative")
	}
	return nil
}

var cronName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func validateCron(jobs map[string]CronJob) error {
	for name, job := range jobs {
		if !cronName.MatchString(name) {
			return keyErr("cron", "invalid job name %q", name)
		}
		if strings.TrimSpace(job.Command) == "" {
			return keyErr("cron."+name+".command", "must not be empty")
		}
		if _, err := schedule.Parse(job.Schedule); err != nil {
			return &Error{Key: "cron." + name + ".schedule", Message: err.Error(), Hint: "use every 5m, every 2h, every 1d or a 5-field cron expression"}
		}
		if job.Timeout < 0 {
			return keyErr("cron."+name+".timeout", "cannot be negative")
		}
	}
	return nil
}

func validateHooks(hooks map[string]Hook) error {
	for name, hook := range hooks {
		if !cronName.MatchString(name) {
			return keyErr("hooks", "invalid hook name %q", name)
		}
		if strings.TrimSpace(hook.Command) == "" {
			return keyErr("hooks."+name+".command", "must not be empty")
		}
		if hook.Timeout < 0 {
			return keyErr("hooks."+name+".timeout", "cannot be negative")
		}
	}
	return nil
}

func parsePrefixes(cidrs []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, &Error{Key: "allow_ips", Message: fmt.Sprintf("invalid entry %q", cidr), Hint: "entries are CIDRs like 10.0.0.0/8; a single address is 203.0.113.1/32"}
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

// Autostart is when an app comes up on its own: true with the host and on resume, false only on
// an explicit run or console start, button only after a POST to its wake page. Any request wakes
// a false app, but only a POST wakes a button app, so crawlers and favicon probes cannot start it.
type Autostart string

const (
	AutostartOn     Autostart = "true"
	AutostartOff    Autostart = "false"
	AutostartButton Autostart = "button"
)

// Starts reports whether the host should bring the app up on boot and on resume.
func (a Autostart) Starts() bool { return a == AutostartOn }

// UnmarshalYAML accepts the booleans true and false and the string "button".
func (a *Autostart) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!bool" {
		*a = AutostartOff
		if node.Value == "true" {
			*a = AutostartOn
		}
		return nil
	}
	switch value := Autostart(node.Value); value {
	case AutostartOn, AutostartOff, AutostartButton:
		*a = value
		return nil
	}
	return &Error{Line: node.Line, Message: "must be true, false or button"}
}

type App struct {
	Procfile      map[string]string  `yaml:"procfile" json:"procfile"`
	Hosts         List               `yaml:"hosts" json:"hosts"`
	WebProcess    string             `yaml:"web_process" json:"web_process"`
	CanonicalHost string             `yaml:"canonical_host" json:"canonical_host"`
	Autostart     Autostart          `yaml:"autostart" json:"autostart"`
	Cron          map[string]CronJob `yaml:"cron" json:"cron"`
	Hooks         map[string]Hook    `yaml:"hooks" json:"hooks"`
	Defaults      `yaml:",inline"`
	Processes     map[string]ProcessOverrides `yaml:"processes" json:"processes"`
}

// CronJob is one named scheduled one-shot command under cron:. Schedule is an "every <n><s|m|h|d>"
// interval or a five-field cron expression. Overlap false skips a run while the previous one is
// still going; a zero Timeout means no limit.
type CronJob struct {
	Schedule string   `yaml:"schedule" json:"schedule"`
	Command  string   `yaml:"command" json:"command"`
	Timeout  Duration `yaml:"timeout" json:"timeout"`
	Overlap  bool     `yaml:"overlap" json:"overlap"`
	Disabled bool     `yaml:"disabled" json:"disabled"`
}

// Hook is one named one-shot command triggered by a signed HTTP ping to
// /hooks/<app>/<hook>. Restart restarts the app when the command exits 0. Secret is the token
// the caller must present; when empty, dboss generates one under state_dir and it never
// belongs in the committed config.
type Hook struct {
	Command  string   `yaml:"command" json:"command"`
	Timeout  Duration `yaml:"timeout" json:"timeout"`
	Restart  bool     `yaml:"restart" json:"restart"`
	Overlap  bool     `yaml:"overlap" json:"overlap"`
	Disabled bool     `yaml:"disabled" json:"disabled"`
	Secret   string   `yaml:"secret" json:"-"`
}

type appFile struct {
	Procfile      map[string]string  `yaml:"procfile"`
	Hosts         List               `yaml:"hosts"`
	WebProcess    string             `yaml:"web_process"`
	CanonicalHost string             `yaml:"canonical_host"`
	Autostart     Autostart          `yaml:"autostart"`
	Cron          map[string]CronJob `yaml:"cron"`
	Hooks         map[string]Hook    `yaml:"hooks"`
	Overrides     `yaml:",inline"`
	Processes     map[string]ProcessOverrides `yaml:"processes"`
}

// LoadApp reads an app's dboss.yaml under a host. Host keys are rejected here because only the
// root file dboss start was pointed at owns the proxy, ports and runtime directories.
func LoadApp(path string, defaults Defaults) (App, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return App{}, err
	}
	return ParseApp(data, path, defaults)
}

// ParseApp is LoadApp on bytes already in memory.
func ParseApp(data []byte, path string, defaults Defaults) (App, error) {
	raw := file{Config: Default()}
	keys, root, err := decode(data, path, &raw)
	if err != nil {
		return App{}, err
	}
	for _, key := range hostKeys {
		if keys[key] {
			return App{}, located(keyErr(key, "is only valid in the root %s", FileName), path, root)
		}
	}
	app, err := buildApp(raw.appFile, defaults)
	if err != nil {
		return App{}, located(err, path, root)
	}
	return app, nil
}

func buildApp(raw appFile, defaults Defaults) (App, error) {
	if len(raw.Procfile) == 0 {
		return App{}, &Error{Key: "procfile", Message: "must contain at least one process", Hint: "e.g. procfile:\n    web: bundle exec puma"}
	}
	app := App{Procfile: raw.Procfile, Hosts: raw.Hosts, WebProcess: "web", CanonicalHost: raw.CanonicalHost, Autostart: AutostartOn, Cron: raw.Cron, Hooks: raw.Hooks, Defaults: defaults, Processes: raw.Processes}
	if raw.WebProcess != "" {
		app.WebProcess = raw.WebProcess
	}
	if raw.Autostart != "" {
		app.Autostart = raw.Autostart
	}
	if app.Processes == nil {
		app.Processes = map[string]ProcessOverrides{}
	}
	apply(&app.Defaults, raw.Overrides)
	if err := validateDefaults(app.Defaults); err != nil {
		return App{}, err
	}
	if err := validateCron(app.Cron); err != nil {
		return App{}, err
	}
	if err := validateHooks(app.Hooks); err != nil {
		return App{}, err
	}
	app.allowPrefixes, _ = parsePrefixes(app.AllowIPs)
	if app.CanonicalHost != "" && !hostAllowed(app.CanonicalHost, app.Hosts) {
		return App{}, keyErr("canonical_host", "%q is not one of hosts %v", app.CanonicalHost, app.Hosts)
	}
	for name := range app.Processes {
		if err := validateProcess(app.Process(name)); err != nil {
			return App{}, scoped(err, "processes."+name)
		}
	}
	return app, nil
}

// Process returns the process keys for name with its processes.<name> overrides applied.
func (a App) Process(name string) Process {
	p := a.Defaults.Process
	if o, ok := a.Processes[name]; ok {
		apply(&p, o)
	}
	return p
}

// MatchHost scores a concrete host against a hosts pattern. A leading "*." matches subdomains
// only, a leading "." matches the bare domain and every subdomain, and a plain pattern must be an
// exact match. The score is the pattern length, so a longer, more specific match wins.
func MatchHost(host, pattern string) (int, bool) {
	if host == pattern {
		return len(pattern) + 10000, true
	}
	if strings.HasPrefix(pattern, ".") {
		if host == pattern[1:] {
			return len(pattern) + 10000, true
		}
		return len(pattern), strings.HasSuffix(host, pattern) && len(host) > len(pattern)
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		return len(suffix), strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	return 0, false
}

// BaseHost strips a leading "*." or "." from a hosts pattern so it can be used as a concrete
// hostname in a Host header or a link.
func BaseHost(pattern string) string {
	return strings.TrimPrefix(strings.TrimPrefix(pattern, "*"), ".")
}

func hostAllowed(host string, patterns []string) bool {
	host = strings.ToLower(host)
	for _, pattern := range patterns {
		if _, ok := MatchHost(host, strings.ToLower(pattern)); ok {
			return true
		}
	}
	return false
}
