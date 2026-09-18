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
	"slices"
	"strconv"
	"strings"
	"time"

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
}

// Proxy has one listener per Listen address; every listener serves the same routing.
type Proxy struct {
	Listen          List     `yaml:"listen" json:"listen"`
	TrustedCIDRs    List     `yaml:"trusted_cidrs" json:"trusted_cidrs"`
	ClientIPHeaders List     `yaml:"client_ip_headers" json:"client_ip_headers"`
	Wake            Wake     `yaml:"wake" json:"wake"`
	Upstream        Upstream `yaml:"upstream" json:"upstream"`
}

// Management is served by the proxy listener; any of the Host names selects the console.
type Management struct {
	Host List           `yaml:"host" json:"host"`
	URL  string         `yaml:"url" json:"url"`
	Auth ManagementAuth `yaml:"auth" json:"auth"`
}

func (m Management) Enabled() bool { return len(m.Host) > 0 }

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
	IdleStop       Duration          `yaml:"idle_stop" json:"idle_stop"`
	Health         string            `yaml:"health" json:"health"`
	HealthInterval Duration          `yaml:"health_interval" json:"health_interval"`
	HealthTimeout  Duration          `yaml:"health_timeout" json:"health_timeout"`
	StopTimeout    Duration          `yaml:"stop_timeout" json:"stop_timeout"`
	StopSignal     string            `yaml:"stop_signal" json:"stop_signal"`
	Restart        string            `yaml:"restart" json:"restart"`
	MaxRestarts    int               `yaml:"max_restarts" json:"max_restarts"`
	RestartReset   Duration          `yaml:"restart_reset" json:"restart_reset"`
	RestartBackoff []any             `yaml:"restart_backoff" json:"restart_backoff"`
	LogMaxSize     Size              `yaml:"log_max_size" json:"log_max_size"`
	LogKeep        int               `yaml:"log_keep" json:"log_keep"`
	LogTailLines   int               `yaml:"log_tail_lines" json:"log_tail_lines"`
	LogRetention   Duration          `yaml:"log_retention" json:"log_retention"`
	LogFlush       Duration          `yaml:"log_flush" json:"log_flush"`
	Shell          bool              `yaml:"shell" json:"shell"`
	Env            map[string]string `yaml:"env" json:"env"`
	Resources      string            `yaml:"resources" json:"resources"`
	MemoryMax      Size              `yaml:"memory_max" json:"memory_max"`
	CPUMax         int               `yaml:"cpu_max" json:"cpu_max"`
}

// Web drives the proxy in front of the app. BasicAuth never leaves the process as JSON so the
// hashes stay out of the console and `dboss status --json`.
type Web struct {
	Static          string            `yaml:"static" json:"static"`
	StaticImmutable List              `yaml:"static_immutable" json:"static_immutable"`
	MaxBody         Size              `yaml:"max_body" json:"max_body"`
	BasicAuth       map[string]string `yaml:"basic_auth" json:"-"`
	AllowIPs        List              `yaml:"allow_ips" json:"allow_ips"`
	Headers         map[string]string `yaml:"headers" json:"headers"`
	MaintenancePage string            `yaml:"maintenance_page" json:"maintenance_page"`
	allowPrefixes   []netip.Prefix
}

// AllowPrefixes is allow_ips parsed at load time; empty means every client is allowed.
func (w Web) AllowPrefixes() []netip.Prefix { return w.allowPrefixes }

type Daemon struct {
	IdleTick      Duration `yaml:"idle_tick" json:"idle_tick"`
	ResumeRunning bool     `yaml:"resume_running" json:"resume_running"`
	PruneAt       string   `yaml:"prune_at" json:"prune_at"`
	LogLevel      string   `yaml:"log_level" json:"log_level"`
}

func Default() Config {
	return Config{
		StateDir: ".dboss/state", LogDir: ".dboss/log", Socket: ".dboss/dboss.sock",
		Proxy:      Proxy{Listen: List{":80"}, ClientIPHeaders: List{"CF-Connecting-IP", "X-Forwarded-For"}, Wake: Wake{RetryAfter: 5, StartingPage: "web/starting.html", CrashedPage: "web/crashed.html", UnknownPage: "web/404.html"}, Upstream: Upstream{DialTimeout: Duration(2 * time.Second), ResponseHeaderTimeout: Duration(60 * time.Second), IdleConnTimeout: Duration(90 * time.Second), MaxIdleConnsPerApp: 32}},
		Management: Management{Auth: ManagementAuth{Realm: "auth.authcog.com", SessionTTL: Duration(24 * time.Hour)}},
		Ports:      Ports{Range: [2]int{3100, 3990}},
		Defaults:   Defaults{Process: Process{IdleStop: Duration(6 * time.Hour), Health: "tcp", HealthInterval: Duration(500 * time.Millisecond), HealthTimeout: Duration(60 * time.Second), StopTimeout: Duration(20 * time.Second), StopSignal: "TERM", Restart: "on-failure", MaxRestarts: 5, RestartReset: Duration(60 * time.Second), RestartBackoff: []any{"1s", 2.0, "60s"}, LogMaxSize: Size(10 << 20), LogKeep: 5, LogTailLines: 500, LogRetention: Duration(720 * time.Hour), LogFlush: Duration(time.Second), Env: map[string]string{}, Resources: "auto"}, Web: Web{StaticImmutable: List{"/assets/"}, BasicAuth: map[string]string{}, Headers: map[string]string{}}},
		Daemon:     Daemon{IdleTick: Duration(time.Minute), ResumeRunning: true, PruneAt: "04:10", LogLevel: "info"},
	}
}

// file is the full dboss.yaml schema: host keys plus app keys. Which role the file plays
// is decided after decoding from whether procfile or apps is present.
type file struct {
	Config  `yaml:",inline"`
	appFile `yaml:",inline"`
}

var hostKeys = []string{"apps", "state_dir", "log_dir", "socket", "proxy", "management", "ports", "defaults", "daemon"}

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
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, located(err, path, &root)
	}
	keys := map[string]bool{}
	if len(root.Content) > 0 && root.Content[0].Kind == yaml.MappingNode {
		for i := 0; i+1 < len(root.Content[0].Content); i += 2 {
			keys[root.Content[0].Content[i].Value] = true
		}
	}
	return keys, &root, nil
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
	if hasApp && cfg.Apps != "" {
		return Config{}, located(&Error{Message: "a file is either an app (procfile) or a host (apps), not both", Hint: "move the host keys to the root dboss.yaml or drop apps"}, path, root)
	}
	if !hasApp && cfg.Apps == "" {
		return Config{}, located(&Error{Message: "needs procfile (an app) or apps (a host)", Hint: "an app file starts with procfile:, a host file with apps: ./apps"}, path, root)
	}
	cfg.Apps = resolvePath(cfg.Dir, cfg.Apps)
	cfg.StateDir = resolvePath(cfg.Dir, cfg.StateDir)
	cfg.LogDir = resolvePath(cfg.Dir, cfg.LogDir)
	cfg.Socket = resolvePath(cfg.Dir, cfg.Socket)
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
	if c.Daemon.IdleTick <= 0 {
		return keyErr("daemon.idle_tick", "must be positive")
	}
	if c.Daemon.LogLevel != "debug" && c.Daemon.LogLevel != "info" && c.Daemon.LogLevel != "warn" && c.Daemon.LogLevel != "error" {
		return keyErr("daemon.log_level", "must be debug, info, warn or error, not %q", c.Daemon.LogLevel)
	}
	if c.Proxy.Upstream.DialTimeout <= 0 || c.Proxy.Upstream.ResponseHeaderTimeout <= 0 || c.Proxy.Upstream.IdleConnTimeout <= 0 || c.Proxy.Upstream.MaxIdleConnsPerApp <= 0 {
		return keyErr("proxy.upstream", "timeouts and max_idle_conns_per_app must be positive")
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
			return &Error{Key: "url", Message: fmt.Sprintf("invalid URL %q", management.URL), Hint: "use the address operators open, e.g. https://boss.example.com"}
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
	for key, value := range map[string]int{"max_restarts": d.MaxRestarts, "log_keep": d.LogKeep, "log_tail_lines": d.LogTailLines} {
		if value < 0 {
			return keyErr(key, "cannot be negative")
		}
	}
	for key, value := range map[string]Duration{"health_interval": d.HealthInterval, "health_timeout": d.HealthTimeout, "restart_reset": d.RestartReset, "log_flush": d.LogFlush} {
		if value <= 0 {
			return keyErr(key, "must be positive")
		}
	}
	for key, value := range map[string]Duration{"stop_timeout": d.StopTimeout, "idle_stop": d.IdleStop, "log_retention": d.LogRetention} {
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

type App struct {
	Procfile      map[string]string `yaml:"procfile" json:"procfile"`
	Hosts         List              `yaml:"hosts" json:"hosts"`
	WebProcess    string            `yaml:"web_process" json:"web_process"`
	CanonicalHost string            `yaml:"canonical_host" json:"canonical_host"`
	Defaults      `yaml:",inline"`
	Processes     map[string]ProcessOverrides `yaml:"processes" json:"processes"`
}

type appFile struct {
	Procfile      map[string]string `yaml:"procfile"`
	Hosts         List              `yaml:"hosts"`
	WebProcess    string            `yaml:"web_process"`
	CanonicalHost string            `yaml:"canonical_host"`
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
	app := App{Procfile: raw.Procfile, Hosts: raw.Hosts, WebProcess: "web", CanonicalHost: raw.CanonicalHost, Defaults: defaults, Processes: raw.Processes}
	if raw.WebProcess != "" {
		app.WebProcess = raw.WebProcess
	}
	if app.Processes == nil {
		app.Processes = map[string]ProcessOverrides{}
	}
	apply(&app.Defaults, raw.Overrides)
	if err := validateDefaults(app.Defaults); err != nil {
		return App{}, err
	}
	app.allowPrefixes, _ = parsePrefixes(app.AllowIPs)
	if app.CanonicalHost != "" && !slices.Contains(app.Hosts, app.CanonicalHost) {
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
