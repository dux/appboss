package config

import (
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"dboss/internal/notify"

	"golang.org/x/crypto/bcrypt"
)

func (c Config) Validate() error { return c.validate(c.Dev()) }

func (c Config) validate(hasApp bool) error {
	if c.Apps == "" && !hasApp {
		return &Error{Message: "apps directory or procfile is required"}
	}
	if c.StateDir == "" || c.LogDir == "" || c.Socket == "" {
		return &Error{Message: "state_dir, log_dir, and socket are required"}
	}
	if err := validateManagement(c.Management, len(c.Proxy.Listen) > 0, hasApp); err != nil {
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
	if c.Daemon.LogFlush <= 0 {
		return keyErr("daemon.log_flush", "must be positive")
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
	if err := validatePostgres(c.Postgres); err != nil {
		return scoped(err, "postgres")
	}
	return nil
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

// Rotation returns the dump rotation window for one database: week, month, or empty when the
// database is not selected.
func (b PostgresBackup) Rotation(name string) string {
	return b.Databases[name].Rotation
}

var databaseName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}$`)

func validatePostgres(p Postgres) error {
	if !p.Enabled {
		return nil
	}
	for _, name := range p.Backup.Selected() {
		if !databaseName.MatchString(name) {
			return &Error{Key: "backup.databases", Message: fmt.Sprintf("invalid database name %q", name), Hint: "names match [A-Za-z_][A-Za-z0-9_$]*, e.g. myapp_production"}
		}
		if rotation := p.Backup.Databases[name].Rotation; rotation != "" && rotation != "week" && rotation != "month" {
			return keyErr("backup.databases."+name+".rotation", "must be week or month, not %q", rotation)
		}
	}
	return nil
}

var notifyFormats = map[string]bool{"generic": true, "slack": true, "discord": true, "ntfy": true}

func validateNotify(n Notify) error {
	if !notifyFormats[n.Format] {
		return keyErr("format", "must be generic, slack, discord or ntfy, not %q", n.Format)
	}
	for _, event := range n.Events {
		if !slices.Contains(notify.Events, event) {
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

func validateManagement(management Management, proxyEnabled, dev bool) error {
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
	// A dev session signs a loopback request in by itself, so the admin list is optional there:
	// it only ever governs AuthCog, which nobody reaches on a local run.
	if len(management.Auth.AdminEmails) == 0 && !dev {
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

// validHostPattern accepts a hostname, optionally led by "*." (subdomains) or "." (apex and
// subdomains).
func validHostPattern(pattern string) bool {
	pattern = NormalizePattern(pattern)
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		pattern = rest
	} else {
		pattern = strings.TrimPrefix(pattern, ".")
	}
	return validHostname(pattern)
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
	for key, value := range map[string]Duration{"health_interval": d.HealthInterval, "liveness_interval": d.LivenessInterval, "health_timeout": d.HealthTimeout, "restart_reset": d.RestartReset} {
		if value <= 0 {
			return keyErr(key, "must be positive")
		}
	}
	for key, value := range map[string]Duration{"stop_timeout": d.StopTimeout, "idle_stop": d.IdleStop, "log_retention": d.LogRetention, "stdout_retention": d.StdoutRetention, "tmp_clean": d.TmpClean} {
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
	for _, extension := range w.StaticExtensions {
		if extension == "" || extension != strings.ToLower(extension) || strings.ContainsAny(extension, "./ ") {
			return keyErr("static_extensions", "%q must be a lowercase extension without the dot", extension)
		}
	}
	if err := validateAlerts(w.Alerts); err != nil {
		return err
	}
	if err := validateAuth(w.Auth); err != nil {
		return err
	}
	return validateAuthCog(w)
}

func validateAuth(a Auth) error {
	if a.SessionTTL <= 0 {
		return keyErr("auth.session_ttl", "must be positive")
	}
	seen := map[string]bool{}
	for _, entry := range a.AllowEmails {
		if entry == "*" {
			// any signed-in AuthCog account
		} else if domain, pattern := strings.CutPrefix(entry, "*@"); pattern {
			if !validHostname(domain) {
				return keyErr("auth.allow_emails", "invalid domain pattern %q", entry)
			}
		} else if address, err := mail.ParseAddress(entry); err != nil || !strings.EqualFold(address.Address, entry) {
			return &Error{Key: "auth.allow_emails", Message: fmt.Sprintf("invalid entry %q", entry), Hint: "use an address like ana@example.com, a whole domain like *@example.com, or * for anyone"}
		}
		if seen[strings.ToLower(entry)] {
			return keyErr("auth.allow_emails", "duplicate entry %q", entry)
		}
		seen[strings.ToLower(entry)] = true
	}
	return nil
}

func validateAlerts(a Alerts) error {
	if a.Window <= 0 {
		return keyErr("alerts.window", "must be positive")
	}
	if a.MinRequests < 0 {
		return keyErr("alerts.min_requests", "cannot be negative")
	}
	if a.ErrorRate < 0 || a.ErrorRate > 100 {
		return keyErr("alerts.error_rate", "must be a percent between 0 and 100")
	}
	if a.SlowP95 < 0 {
		return keyErr("alerts.slow_p95", "cannot be negative")
	}
	return nil
}

func validatePubsub(p Pubsub, prefix string) error {
	if err := validateURLPath(prefix+".path", p.Path); err != nil {
		return err
	}
	for key, value := range map[string]int{"replay": p.Replay, "max_clients": p.MaxClients} {
		if value < 0 {
			return keyErr(prefix+"."+key, "cannot be negative")
		}
	}
	if p.MaxMessageSize < 0 {
		return keyErr(prefix+".max_message_size", "cannot be negative")
	}
	return nil
}

// authcogRealm is the subdomain before authcog.com, e.g. "auth" or "dboss".
var authcogRealm = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func validateAuthCog(w Web) error {
	if !w.AuthCog.Enabled() {
		return nil
	}
	if err := validateURLPath("authcog.path", w.AuthCog.Path); err != nil {
		return err
	}
	if w.AuthCog.Path == "" {
		return keyErr("authcog.path", "must not be empty when login is on")
	}
	if !authcogRealm.MatchString(w.AuthCog.Realm) {
		return &Error{Key: "authcog.realm", Message: fmt.Sprintf("invalid realm %q", w.AuthCog.Realm), Hint: "use one DNS label such as auth (auth.authcog.com) or dboss"}
	}
	if w.HealthEndpoint != "" && w.HealthEndpoint == w.AuthCog.Path {
		return keyErr("authcog.path", "must differ from health_endpoint %q", w.HealthEndpoint)
	}
	return nil
}

// validateURLPath checks the URL prefix shape shared by the pubsub and authcog paths.
func validateURLPath(key, value string) error {
	if value == "" {
		return nil
	}
	invalid := !strings.HasPrefix(value, "/") || value == "/" || strings.HasSuffix(value, "/") || strings.Contains(value, "//")
	for _, character := range value {
		if !(character == '/' || character == '-' || character == '_' || character == '.' || character == '~' ||
			character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9') {
			invalid = true
		}
	}
	if invalid {
		return &Error{Key: key, Message: fmt.Sprintf("invalid path %q", value), Hint: "use a URL prefix such as /socketio (letters, digits, - _ . ~ and / only)"}
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
