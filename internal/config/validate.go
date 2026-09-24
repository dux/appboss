package config

import (
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"dboss/internal/events"
	"dboss/internal/notify"
)

func (c Config) Validate() error { return c.validate(c.Dev()) }

func (c Config) validate(hasApp bool) error {
	if c.Apps == "" && !hasApp {
		return &Error{Message: "apps directory or procfile is required"}
	}
	if c.RuntimeDir == "" {
		return keyErr("dir", "is required")
	}
	if err := validateManagement(c.Management, len(c.Proxy.Listen) > 0, hasApp); err != nil {
		return scoped(err, "management")
	}
	if !validHostname(c.AuthCogRealm) {
		return keyErr("authcog_realm", "invalid hostname %q", c.AuthCogRealm)
	}
	if c.Ports[0] < 1 || c.Ports[1] > 65535 || c.Ports[0] > c.Ports[1] {
		return &Error{Key: "ports", Message: fmt.Sprintf("invalid range %v", c.Ports), Hint: "give [first, last] between 1 and 65535, e.g. [3100, 3990]"}
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
		if port >= c.Ports[0] && port <= c.Ports[1] {
			return keyErr("proxy.listen", "port %d overlaps ports", port)
		}
		if listeners[address] {
			return keyErr("proxy.listen", "duplicate entry %q", address)
		}
		listeners[address] = true
	}
	if c.Proxy.Timeout <= 0 {
		return keyErr("proxy.timeout", "must be positive")
	}
	if err := validateDefaults(c.Defaults); err != nil {
		return scoped(err, "defaults")
	}
	if _, err := time.Parse("15:04", c.MaintenanceAt); err != nil {
		return &Error{Key: "maintenance_at", Message: fmt.Sprintf("invalid time %q", c.MaintenanceAt), Hint: "use 24h clock HH:MM, e.g. \"04:10\""}
	}
	if c.DiskAlert < 0 || c.DiskAlert > 100 {
		return keyErr("disk_alert", "must be a percent between 0 and 100")
	}
	if c.AuditRetention < 0 {
		return keyErr("audit_retention", "cannot be negative")
	}
	if c.LogLevel != "debug" && c.LogLevel != "info" && c.LogLevel != "warn" && c.LogLevel != "error" {
		return keyErr("log_level", "must be debug, info, warn or error, not %q", c.LogLevel)
	}
	if err := validateNotify(c.Notify); err != nil {
		return scoped(err, "notify")
	}
	if err := validatePostgres(c.Postgres); err != nil {
		return scoped(err, "postgres")
	}
	return nil
}

func validateNotify(n Notify) error {
	for _, event := range n.Events {
		if !slices.Contains(notify.Events, event) {
			return keyErr("events", "unknown event %q", event)
		}
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
		if len(management.Admins) > 0 {
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
	// A dev session signs a loopback request in by itself, so the admin list is optional there:
	// it only ever governs AuthCog, which nobody reaches on a local run.
	if len(management.Admins) == 0 && !dev {
		return keyErr("admins", "must contain at least one email")
	}
	emails := map[string]bool{}
	for _, email := range management.Admins {
		address, err := mail.ParseAddress(email)
		if err != nil || !strings.EqualFold(address.Address, email) {
			return keyErr("admins", "invalid entry %q", email)
		}
		normalized := strings.ToLower(address.Address)
		if emails[normalized] {
			return keyErr("admins", "duplicate entry %q", email)
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
	for key, value := range map[string]int{"max_restarts": d.MaxRestarts, "unhealthy_threshold": d.UnhealthyThreshold} {
		if value < 0 {
			return keyErr(key, "cannot be negative")
		}
	}
	for key, value := range map[string]Duration{"liveness_interval": d.LivenessInterval, "health_timeout": d.HealthTimeout} {
		if value <= 0 {
			return keyErr(key, "must be positive")
		}
	}
	for key, value := range map[string]Duration{"stop_timeout": d.StopTimeout, "idle_stop": d.IdleStop, "log_retention": d.LogRetention, "stdout_retention": d.StdoutRetention, "tmp_clean": d.TmpClean} {
		if value < 0 {
			return keyErr(key, "cannot be negative")
		}
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
	for user, password := range w.BasicAuth {
		if user == "" || strings.ContainsAny(user, ": ") {
			return keyErr("basic_auth", "invalid user %q", user)
		}
		if password == "" {
			return &Error{Key: "basic_auth." + user, Message: "password is empty"}
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
	if err := validateEvents(w.Events); err != nil {
		return err
	}
	if err := validateAuth(w); err != nil {
		return err
	}
	return validateAuthCog(w)
}

func validateAuth(w Web) error {
	if w.SessionTTL <= 0 {
		return keyErr("session_ttl", "must be positive")
	}
	seen := map[string]bool{}
	for _, entry := range w.Auth {
		if entry == "*" {
			// any signed-in AuthCog account
		} else if domain, pattern := strings.CutPrefix(entry, "*@"); pattern {
			if !validHostname(domain) {
				return keyErr("auth", "invalid domain pattern %q", entry)
			}
		} else if address, err := mail.ParseAddress(entry); err != nil || !strings.EqualFold(address.Address, entry) {
			return &Error{Key: "auth", Message: fmt.Sprintf("invalid entry %q", entry), Hint: "use an address like ana@example.com, a whole domain like *@example.com, or * for anyone"}
		}
		if seen[strings.ToLower(entry)] {
			return keyErr("auth", "duplicate entry %q", entry)
		}
		seen[strings.ToLower(entry)] = true
	}
	return nil
}

func validateAlerts(a Alerts) error {
	if a.ErrorRate < 0 || a.ErrorRate > 100 {
		return keyErr("alerts.error_rate", "must be a percent between 0 and 100")
	}
	if a.SlowP95 < 0 {
		return keyErr("alerts.slow_p95", "cannot be negative")
	}
	return nil
}

func validateEvents(e Events) error {
	if e.Retention < 0 {
		return keyErr("events.retention", "cannot be negative")
	}
	for name, filter := range e.Views {
		if err := events.ValidateView(events.View{Name: name, Filter: filter}); err != nil {
			return keyErr("events.views."+name, "%v", err)
		}
	}
	for name, funnel := range e.Funnels {
		converted := EventFunnelOf(name, funnel)
		if err := events.ValidateFunnel(&converted); err != nil {
			return keyErr("events.funnels."+name, "%v", err)
		}
	}
	return nil
}

// EventFunnelOf converts a configured funnel to the events package's shape.
func EventFunnelOf(name string, funnel EventFunnel) events.Funnel {
	steps := make([]events.Step, len(funnel.Steps))
	for i, step := range funnel.Steps {
		steps[i] = events.Step{Name: step.Name, Filter: step.Filter}
	}
	return events.Funnel{Name: name, By: funnel.By, Window: funnel.Window.Value(), Breakdown: funnel.Breakdown, Steps: steps, Source: events.SourceYAML}
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

func validateAuthCog(w Web) error {
	if !w.AuthCog.Enabled() {
		return nil
	}
	if err := validateURLPath("authcog", string(w.AuthCog)); err != nil {
		return err
	}
	if w.HealthEndpoint != "" && w.HealthEndpoint == string(w.AuthCog) {
		return keyErr("authcog", "must differ from health_endpoint %q", w.HealthEndpoint)
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
