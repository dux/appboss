package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	v, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
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
		return err
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
			multiplier = 0
		}
	}
	if multiplier == 0 {
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

type Config struct {
	SourcePath string     `yaml:"-" json:"-"`
	Apps       []string   `yaml:"apps" json:"apps"`
	StateDir   string     `yaml:"state_dir" json:"state_dir"`
	LogDir     string     `yaml:"log_dir" json:"log_dir"`
	Socket     string     `yaml:"socket" json:"socket"`
	Proxy      Proxy      `yaml:"proxy" json:"proxy"`
	Management Management `yaml:"management" json:"management"`
	Ports      Ports      `yaml:"ports" json:"ports"`
	Defaults   Defaults   `yaml:"defaults" json:"defaults"`
	Daemon     Daemon     `yaml:"daemon" json:"daemon"`
}

type Proxy struct {
	Listen          string   `yaml:"listen" json:"listen"`
	TrustedCIDRs    []string `yaml:"trusted_cidrs" json:"trusted_cidrs"`
	ClientIPHeaders []string `yaml:"client_ip_headers" json:"client_ip_headers"`
	Wake            Wake     `yaml:"wake" json:"wake"`
	Upstream        Upstream `yaml:"upstream" json:"upstream"`
}

// Management is served by the proxy listener; the host header selects the console.
type Management struct {
	Host string         `yaml:"host" json:"host"`
	Auth ManagementAuth `yaml:"auth" json:"auth"`
}

func (m Management) Enabled() bool { return m.Host != "" }

type ManagementAuth struct {
	Realm       string   `yaml:"realm" json:"realm"`
	AdminEmails []string `yaml:"admin_emails" json:"admin_emails"`
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

type Defaults struct {
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

type Daemon struct {
	IdleTick      Duration `yaml:"idle_tick" json:"idle_tick"`
	ResumeRunning bool     `yaml:"resume_running" json:"resume_running"`
	PruneAt       string   `yaml:"prune_at" json:"prune_at"`
	Log           string   `yaml:"log" json:"log"`
	LogLevel      string   `yaml:"log_level" json:"log_level"`
}

func Default() Config {
	return Config{
		StateDir: "/var/lib/boss", LogDir: "/var/log/boss", Socket: "/run/boss/boss.sock",
		Proxy:      Proxy{Listen: "127.0.0.1:8080", ClientIPHeaders: []string{"CF-Connecting-IP", "X-Forwarded-For"}, Wake: Wake{RetryAfter: 5, StartingPage: "web/starting.html", CrashedPage: "web/crashed.html", UnknownPage: "web/404.html"}, Upstream: Upstream{DialTimeout: Duration(2 * time.Second), ResponseHeaderTimeout: Duration(60 * time.Second), IdleConnTimeout: Duration(90 * time.Second), MaxIdleConnsPerApp: 32}},
		Management: Management{Auth: ManagementAuth{Realm: "auth.authcog.com", SessionTTL: Duration(24 * time.Hour)}},
		Ports:      Ports{Range: [2]int{3100, 3990}},
		Defaults:   Defaults{IdleStop: Duration(6 * time.Hour), Health: "tcp", HealthInterval: Duration(500 * time.Millisecond), HealthTimeout: Duration(60 * time.Second), StopTimeout: Duration(20 * time.Second), StopSignal: "TERM", Restart: "on-failure", MaxRestarts: 5, RestartReset: Duration(60 * time.Second), RestartBackoff: []any{"1s", 2.0, "60s"}, LogMaxSize: Size(10 << 20), LogKeep: 5, LogTailLines: 500, LogRetention: Duration(720 * time.Hour), LogFlush: Duration(time.Second), Env: map[string]string{}, Resources: "auto"},
		Daemon:     Daemon{IdleTick: Duration(time.Minute), ResumeRunning: true, PruneAt: "04:10", Log: "stderr", LogLevel: "info"},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", path, err)
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	baseDir := filepath.Dir(absolutePath)
	cfg.SourcePath = absolutePath
	for index, appPath := range cfg.Apps {
		cfg.Apps[index] = resolvePath(baseDir, appPath)
	}
	cfg.StateDir = resolvePath(baseDir, cfg.StateDir)
	cfg.LogDir = resolvePath(baseDir, cfg.LogDir)
	cfg.Socket = resolvePath(baseDir, cfg.Socket)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if len(c.Apps) == 0 {
		return errors.New("apps must contain at least one folder")
	}
	paths, names := map[string]bool{}, map[string]bool{}
	for _, appPath := range c.Apps {
		if appPath == "" {
			return errors.New("apps cannot contain an empty folder")
		}
		cleanPath := filepath.Clean(appPath)
		name := filepath.Base(cleanPath)
		if paths[cleanPath] {
			return fmt.Errorf("duplicate app folder %q", appPath)
		}
		if names[name] {
			return fmt.Errorf("duplicate app name %q", name)
		}
		paths[cleanPath], names[name] = true, true
	}
	if c.StateDir == "" || c.LogDir == "" || c.Socket == "" {
		return errors.New("state_dir, log_dir, and socket are required")
	}
	if err := validateManagement(c.Management, c.Proxy.Listen); err != nil {
		return fmt.Errorf("management: %w", err)
	}
	for _, cidr := range c.Proxy.TrustedCIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return fmt.Errorf("proxy.trusted_cidrs: invalid entry %q", cidr)
		}
	}
	if c.Ports.Range[0] < 1 || c.Ports.Range[1] > 65535 || c.Ports.Range[0] > c.Ports.Range[1] {
		return fmt.Errorf("invalid ports.range %v", c.Ports.Range)
	}
	if c.Proxy.Wake.RetryAfter < 1 {
		return errors.New("proxy.wake.retry_after must be positive")
	}
	if c.Proxy.Listen != "" {
		_, portValue, err := net.SplitHostPort(c.Proxy.Listen)
		if err != nil {
			return fmt.Errorf("proxy.listen: %w", err)
		}
		port, err := strconv.Atoi(portValue)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("proxy.listen has invalid port %q", portValue)
		}
		if port >= c.Ports.Range[0] && port <= c.Ports.Range[1] {
			return errors.New("proxy.listen overlaps ports.range")
		}
	}
	if err := validateDefaults(c.Defaults); err != nil {
		return fmt.Errorf("defaults: %w", err)
	}
	if _, err := time.Parse("15:04", c.Daemon.PruneAt); err != nil {
		return fmt.Errorf("daemon.prune_at: %w", err)
	}
	if c.Daemon.IdleTick <= 0 {
		return errors.New("daemon.idle_tick must be positive")
	}
	if c.Daemon.Log != "stderr" && !filepath.IsAbs(c.Daemon.Log) {
		return errors.New("daemon.log must be stderr or an absolute path")
	}
	if c.Daemon.LogLevel != "debug" && c.Daemon.LogLevel != "info" && c.Daemon.LogLevel != "warn" && c.Daemon.LogLevel != "error" {
		return fmt.Errorf("invalid daemon.log_level %q", c.Daemon.LogLevel)
	}
	if c.Proxy.Upstream.DialTimeout <= 0 || c.Proxy.Upstream.ResponseHeaderTimeout <= 0 || c.Proxy.Upstream.IdleConnTimeout <= 0 || c.Proxy.Upstream.MaxIdleConnsPerApp <= 0 {
		return errors.New("proxy.upstream timeouts and max_idle_conns_per_app must be positive")
	}
	return nil
}

func validateManagement(management Management, proxyListen string) error {
	if !management.Enabled() {
		if len(management.Auth.AdminEmails) > 0 {
			return errors.New("host is required when management is configured")
		}
		return nil
	}
	if proxyListen == "" {
		return errors.New("proxy.listen is required because the console is served by the proxy listener")
	}
	if !validHostname(management.Host) {
		return fmt.Errorf("invalid host %q", management.Host)
	}
	if !validHostname(management.Auth.Realm) {
		return fmt.Errorf("invalid auth.realm %q", management.Auth.Realm)
	}
	if management.Auth.SessionTTL <= 0 {
		return errors.New("auth.session_ttl must be positive")
	}
	if len(management.Auth.AdminEmails) == 0 {
		return errors.New("auth.admin_emails must contain at least one email")
	}
	emails := map[string]bool{}
	for _, email := range management.Auth.AdminEmails {
		address, err := mail.ParseAddress(email)
		if err != nil || !strings.EqualFold(address.Address, email) {
			return fmt.Errorf("invalid auth.admin_emails entry %q", email)
		}
		normalized := strings.ToLower(address.Address)
		if emails[normalized] {
			return fmt.Errorf("duplicate auth.admin_emails entry %q", email)
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
	if d.Health != "tcp" && !strings.HasPrefix(d.Health, "http:/") {
		return fmt.Errorf("health must be tcp or http:/path")
	}
	if d.Restart != "on-failure" && d.Restart != "always" && d.Restart != "never" {
		return fmt.Errorf("restart must be on-failure, always, or never")
	}
	validSignals := map[string]bool{"TERM": true, "INT": true, "QUIT": true, "USR1": true, "USR2": true}
	if !validSignals[d.StopSignal] {
		return fmt.Errorf("invalid stop_signal %q", d.StopSignal)
	}
	if d.MaxRestarts < 0 || d.LogKeep < 0 || d.LogTailLines < 0 {
		return errors.New("counts cannot be negative")
	}
	if d.HealthInterval <= 0 || d.HealthTimeout <= 0 || d.StopTimeout < 0 || d.RestartReset <= 0 || d.LogFlush <= 0 || d.IdleStop < 0 || d.LogRetention < 0 {
		return errors.New("durations are outside their allowed range")
	}
	if d.Resources != "auto" && d.Resources != "procgroup" && d.Resources != "cgroup" {
		return fmt.Errorf("invalid resources %q", d.Resources)
	}
	if len(d.RestartBackoff) != 3 {
		return errors.New("restart_backoff must contain first delay, multiplier, and cap")
	}
	first, err := time.ParseDuration(fmt.Sprint(d.RestartBackoff[0]))
	if err != nil || first <= 0 {
		return errors.New("restart_backoff first delay must be positive")
	}
	multiplier, err := strconv.ParseFloat(fmt.Sprint(d.RestartBackoff[1]), 64)
	if err != nil || multiplier < 1 {
		return errors.New("restart_backoff multiplier must be at least 1")
	}
	maximum, err := time.ParseDuration(fmt.Sprint(d.RestartBackoff[2]))
	if err != nil || maximum < first {
		return errors.New("restart_backoff cap must be at least the first delay")
	}
	return nil
}

type App struct {
	Procfile   map[string]string `yaml:"procfile" json:"procfile"`
	Hosts      []string          `yaml:"hosts" json:"hosts"`
	WebProcess string            `yaml:"web_process" json:"web_process"`
	Defaults
	Processes map[string]ProcessOverrides `yaml:"processes" json:"processes"`
}

type ProcessOverrides struct {
	IdleStop       *Duration         `yaml:"idle_stop" json:"idle_stop"`
	Health         *string           `yaml:"health" json:"health"`
	HealthInterval *Duration         `yaml:"health_interval" json:"health_interval"`
	HealthTimeout  *Duration         `yaml:"health_timeout" json:"health_timeout"`
	StopTimeout    *Duration         `yaml:"stop_timeout" json:"stop_timeout"`
	StopSignal     *string           `yaml:"stop_signal" json:"stop_signal"`
	Restart        *string           `yaml:"restart" json:"restart"`
	MaxRestarts    *int              `yaml:"max_restarts" json:"max_restarts"`
	RestartReset   *Duration         `yaml:"restart_reset" json:"restart_reset"`
	RestartBackoff []any             `yaml:"restart_backoff" json:"restart_backoff"`
	LogMaxSize     *Size             `yaml:"log_max_size" json:"log_max_size"`
	LogKeep        *int              `yaml:"log_keep" json:"log_keep"`
	LogTailLines   *int              `yaml:"log_tail_lines" json:"log_tail_lines"`
	LogRetention   *Duration         `yaml:"log_retention" json:"log_retention"`
	LogFlush       *Duration         `yaml:"log_flush" json:"log_flush"`
	Shell          *bool             `yaml:"shell" json:"shell"`
	Env            map[string]string `yaml:"env" json:"env"`
	Resources      *string           `yaml:"resources" json:"resources"`
	MemoryMax      *Size             `yaml:"memory_max" json:"memory_max"`
	CPUMax         *int              `yaml:"cpu_max" json:"cpu_max"`
}

type appFile struct {
	Procfile       map[string]string           `yaml:"procfile"`
	Hosts          []string                    `yaml:"hosts"`
	WebProcess     string                      `yaml:"web_process"`
	IdleStop       *Duration                   `yaml:"idle_stop"`
	Health         *string                     `yaml:"health"`
	HealthInterval *Duration                   `yaml:"health_interval"`
	HealthTimeout  *Duration                   `yaml:"health_timeout"`
	StopTimeout    *Duration                   `yaml:"stop_timeout"`
	StopSignal     *string                     `yaml:"stop_signal"`
	Restart        *string                     `yaml:"restart"`
	MaxRestarts    *int                        `yaml:"max_restarts"`
	RestartReset   *Duration                   `yaml:"restart_reset"`
	RestartBackoff []any                       `yaml:"restart_backoff"`
	LogMaxSize     *Size                       `yaml:"log_max_size"`
	LogKeep        *int                        `yaml:"log_keep"`
	LogTailLines   *int                        `yaml:"log_tail_lines"`
	Shell          *bool                       `yaml:"shell"`
	LogRetention   *Duration                   `yaml:"log_retention"`
	LogFlush       *Duration                   `yaml:"log_flush"`
	Resources      *string                     `yaml:"resources"`
	MemoryMax      *Size                       `yaml:"memory_max"`
	CPUMax         *int                        `yaml:"cpu_max"`
	Env            map[string]string           `yaml:"env"`
	Processes      map[string]ProcessOverrides `yaml:"processes"`
}

func LoadApp(path string, defaults Defaults) (App, error) {
	app := App{WebProcess: "web", Defaults: defaults, Processes: map[string]ProcessOverrides{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return App{}, err
	}
	var raw appFile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return App{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if len(raw.Procfile) == 0 {
		return App{}, errors.New("procfile must contain at least one process")
	}
	app.Procfile, app.Hosts, app.Processes = raw.Procfile, raw.Hosts, raw.Processes
	if raw.WebProcess != "" {
		app.WebProcess = raw.WebProcess
	}
	if raw.IdleStop != nil {
		app.IdleStop = *raw.IdleStop
	}
	if raw.Health != nil {
		app.Health = *raw.Health
	}
	if raw.HealthInterval != nil {
		app.HealthInterval = *raw.HealthInterval
	}
	if raw.HealthTimeout != nil {
		app.HealthTimeout = *raw.HealthTimeout
	}
	if raw.StopTimeout != nil {
		app.StopTimeout = *raw.StopTimeout
	}
	if raw.StopSignal != nil {
		app.StopSignal = *raw.StopSignal
	}
	if raw.Restart != nil {
		app.Restart = *raw.Restart
	}
	if raw.MaxRestarts != nil {
		app.MaxRestarts = *raw.MaxRestarts
	}
	if raw.RestartReset != nil {
		app.RestartReset = *raw.RestartReset
	}
	if raw.RestartBackoff != nil {
		app.RestartBackoff = raw.RestartBackoff
	}
	if raw.LogMaxSize != nil {
		app.LogMaxSize = *raw.LogMaxSize
	}
	if raw.LogKeep != nil {
		app.LogKeep = *raw.LogKeep
	}
	if raw.LogTailLines != nil {
		app.LogTailLines = *raw.LogTailLines
	}
	if raw.Shell != nil {
		app.Shell = *raw.Shell
	}
	if raw.LogRetention != nil {
		app.LogRetention = *raw.LogRetention
	}
	if raw.LogFlush != nil {
		app.LogFlush = *raw.LogFlush
	}
	if raw.Resources != nil {
		app.Resources = *raw.Resources
	}
	if raw.MemoryMax != nil {
		app.MemoryMax = *raw.MemoryMax
	}
	if raw.CPUMax != nil {
		app.CPUMax = *raw.CPUMax
	}
	if raw.Env != nil {
		app.Env = cloneMap(defaults.Env)
		for k, v := range raw.Env {
			app.Env[k] = v
		}
	}
	if err := validateDefaults(app.Defaults); err != nil {
		return App{}, err
	}
	for name := range app.Processes {
		if err := validateDefaults(app.Process(name)); err != nil {
			return App{}, fmt.Errorf("processes.%s: %w", name, err)
		}
	}
	return app, nil
}

func (a App) Process(name string) Defaults {
	d := a.Defaults
	o, ok := a.Processes[name]
	if !ok {
		return d
	}
	if o.IdleStop != nil {
		d.IdleStop = *o.IdleStop
	}
	if o.Health != nil {
		d.Health = *o.Health
	}
	if o.HealthInterval != nil {
		d.HealthInterval = *o.HealthInterval
	}
	if o.HealthTimeout != nil {
		d.HealthTimeout = *o.HealthTimeout
	}
	if o.StopTimeout != nil {
		d.StopTimeout = *o.StopTimeout
	}
	if o.StopSignal != nil {
		d.StopSignal = *o.StopSignal
	}
	if o.Restart != nil {
		d.Restart = *o.Restart
	}
	if o.MaxRestarts != nil {
		d.MaxRestarts = *o.MaxRestarts
	}
	if o.RestartReset != nil {
		d.RestartReset = *o.RestartReset
	}
	if o.RestartBackoff != nil {
		d.RestartBackoff = o.RestartBackoff
	}
	if o.LogMaxSize != nil {
		d.LogMaxSize = *o.LogMaxSize
	}
	if o.LogKeep != nil {
		d.LogKeep = *o.LogKeep
	}
	if o.LogTailLines != nil {
		d.LogTailLines = *o.LogTailLines
	}
	if o.Shell != nil {
		d.Shell = *o.Shell
	}
	if o.LogRetention != nil {
		d.LogRetention = *o.LogRetention
	}
	if o.LogFlush != nil {
		d.LogFlush = *o.LogFlush
	}
	if o.Resources != nil {
		d.Resources = *o.Resources
	}
	if o.MemoryMax != nil {
		d.MemoryMax = *o.MemoryMax
	}
	if o.CPUMax != nil {
		d.CPUMax = *o.CPUMax
	}
	if o.Env != nil {
		d.Env = cloneMap(d.Env)
		for k, v := range o.Env {
			d.Env[k] = v
		}
	}
	return d
}

func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for k, v := range source {
		result[k] = v
	}
	return result
}

func ResolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
