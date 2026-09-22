package config

// KeySpec is the hand-written metadata for one configuration key. The path, its nesting, its
// Go type and its default come from the config structs and Default(), so they cannot drift; the
// block, name, description, enum and flags are documented here.
type KeySpec struct {
	Block       string   // block id from schema.go
	Name        string   // short human label
	Description string   // one line
	Enum        []string // allowed values, when the key is a fixed set
	Example     string   // shown when the key has no static default
	Required    bool     // unconditional presence only; conditional rules live in validate()
	Secret      bool     // sensitive: mask it and never ship it to a client
}

// keySpecs documents every key. A key missing here fails keys_test.go, and so does a spec that
// names a path no struct exposes.
var keySpecs = map[string]KeySpec{
	// --- Paths ---
	"apps":      {Block: "paths", Name: "Apps directory", Description: "directory of app folders, one entry (folder or symlink) per app", Example: "./apps"},
	"state_dir": {Block: "paths", Name: "State directory", Description: "session state: process state and the console signing key"},
	"log_dir":   {Block: "paths", Name: "Log directory", Description: "process logs and per-app request logs"},
	"socket":    {Block: "paths", Name: "Control socket", Description: "unix socket for the control API the CLI talks to"},

	// --- Proxy ---
	"proxy.listen":                           {Block: "proxy", Name: "Listen addresses", Description: "one or more addresses to listen on; owns port 80 and routes every request to an app, empty disables the proxy", Example: "127.0.0.1:8080"},
	"proxy.trusted_cidrs":                    {Block: "proxy", Name: "Trusted CIDRs", Description: "only these source CIDRs may connect; empty accepts anyone", Example: "[173.245.48.0/20, 2400:cb00::/32]"},
	"proxy.client_ip_headers":                {Block: "proxy", Name: "Client IP headers", Description: "headers checked in order for the real client IP"},
	"proxy.cloudflare_only":                  {Block: "proxy", Name: "Cloudflare only", Description: "accept a request only when it carries Cloudflare's CF-Ray and CF-Connecting-IP headers; headers are spoofable, so also lock the origin to Cloudflare at the firewall"},
	"proxy.tls.listen":                       {Block: "proxy", Name: "TLS listener", Description: "address for the built-in HTTPS listener; empty means TLS terminates elsewhere (Cloudflare)", Example: ":443"},
	"proxy.tls.email":                        {Block: "proxy", Name: "ACME email", Description: "contact email registered with the certificate authority", Example: "ops@example.com"},
	"proxy.tls.directory":                    {Block: "proxy", Name: "ACME directory", Description: "certificate authority directory: empty for Let's Encrypt production, staging for the test endpoint, or a URL", Example: "staging"},
	"proxy.tls.cache_dir":                    {Block: "proxy", Name: "Certificate cache", Description: "directory holding issued certificates; defaults to state_dir/acme", Example: "/var/lib/dboss/state/acme"},
	"proxy.tls.redirect":                     {Block: "proxy", Name: "Redirect to HTTPS", Description: "send plain-HTTP requests to HTTPS; ACME challenges are still answered on port 80"},
	"proxy.wake.retry_after":                 {Block: "proxy", Name: "Wake retry after", Description: "seconds sent in Retry-After and the starting page refresh", Example: "10"},
	"proxy.wake.starting_page":               {Block: "proxy", Name: "Starting page", Description: "page served while an app is stopped or starting"},
	"proxy.wake.crashed_page":                {Block: "proxy", Name: "Crashed page", Description: "page served when an app has crashed"},
	"proxy.wake.unknown_page":                {Block: "proxy", Name: "Unknown host page", Description: "page served when no app matches the host"},
	"proxy.upstream.dial_timeout":            {Block: "proxy", Name: "Dial timeout", Description: "connect timeout to an app port"},
	"proxy.upstream.response_header_timeout": {Block: "proxy", Name: "Response header timeout", Description: "how long an app may take to start responding"},
	"proxy.upstream.idle_conn_timeout":       {Block: "proxy", Name: "Idle connection timeout", Description: "idle keep-alive connection lifetime to an app"},
	"proxy.upstream.max_idle_conns_per_app":  {Block: "proxy", Name: "Max idle connections", Description: "idle keep-alive connections kept per app"},

	// --- Management console ---
	"management.host":              {Block: "management", Name: "Console hosts", Description: "one or more hostnames of the management console; omit to disable it", Example: "dboss.example.com"},
	"management.url":               {Block: "management", Name: "Console URL", Description: "public URL of the console as operators open it; defaults to https://<first management.host>", Example: "https://dboss.example.com"},
	"management.auth.realm":        {Block: "management", Name: "Auth realm", Description: "full AuthCog hostname the console signs in against (not the app authcog.realm label)", Example: "auth.authcog.com"},
	"management.auth.admin_emails": {Block: "management", Name: "Admin emails", Description: "email addresses allowed into the console", Example: "[admin@example.com]"},
	"management.auth.session_ttl":  {Block: "management", Name: "Session lifetime", Description: "signed console session lifetime", Example: "48h"},
	"management.metrics.enabled":   {Block: "management", Name: "Metrics endpoints", Description: "serve /healthz, /readyz and /metrics on the management host"},
	"management.metrics.token":     {Block: "management", Name: "Metrics token", Description: "when set, /metrics requires this as a bearer token; health endpoints stay open", Example: "$METRICS_TOKEN", Secret: true},

	// --- Ports ---
	"ports.range": {Block: "ports", Name: "Port range", Description: "inclusive port range dboss owns; the first port is the console"},

	// --- Daemon ---
	"daemon.idle_tick":           {Block: "daemon", Name: "Idle check interval", Description: "how often idle apps are checked"},
	"daemon.resume_running":      {Block: "daemon", Name: "Resume running apps", Description: "on start, resume the apps in running.json (every app on a first start)"},
	"daemon.prune_at":            {Block: "daemon", Name: "Log prune time", Description: "local time of the daily request-log prune", Example: "03:30"},
	"daemon.vacuum_at":           {Block: "daemon", Name: "Vacuum time", Description: "local time of the daily SQLite VACUUM; empty disables it", Example: "04:00"},
	"daemon.log_level":           {Block: "daemon", Name: "Log level", Description: "dboss's own log level", Enum: []string{"debug", "info", "warn", "error"}},
	"daemon.log_ingest_interval": {Block: "daemon", Name: "Log ingest interval", Description: "how often process log segments are sealed and ingested into the log store"},
	"daemon.audit_retention":     {Block: "daemon", Name: "Audit retention", Description: "how long operator audit rows are kept; 0 keeps them forever", Example: "720h"},

	// --- Notifications ---
	"notify.url":          {Block: "notify", Name: "Webhook URL", Description: "webhook that receives crash and failure events; empty disables notifications", Example: "$ALERT_WEBHOOK_URL"},
	"notify.format":       {Block: "notify", Name: "Format", Description: "webhook payload shape", Enum: []string{"generic", "slack", "discord", "ntfy"}},
	"notify.events":       {Block: "notify", Name: "Events", Description: "events to post: crash, restart-loop, health-timeout, wake-failed, hook-failed, deploy, config-changed, backup-failed, error-rate, slow"},
	"notify.min_interval": {Block: "notify", Name: "Quiet period", Description: "quiet period per app and event, so a crash loop does not spam", Example: "10m"},
	"notify.headers":      {Block: "notify", Name: "Extra headers", Description: "extra headers sent with every webhook request", Example: "{Authorization: \"Bearer $TOKEN\"}"},

	// --- PostgreSQL ---
	"postgres.enabled":          {Block: "postgres", Name: "Enable PostgreSQL", Description: "inspect the host PostgreSQL and run scheduled backups; false hides the console tab"},
	"postgres.dsn":              {Block: "postgres", Name: "Connection string", Description: "libpq connection string or URL; empty auto-detects the local socket then 127.0.0.1 using the PG* environment", Example: "$DATABASE_URL", Secret: true},
	"postgres.backup.databases": {Block: "postgres", Name: "Databases", Description: "databases dumped by the daily run, keyed by name; rotation is week or month (empty means week)", Example: "{myapp_production: {rotation: week}, reports: {rotation: month}}"},

	// --- App ---
	"procfile":  {Block: "app", Name: "Process commands", Description: "process commands by name; names match [a-z][a-z0-9_-]*. A scalar is a background process; every mapping that adds hosts is a web process (an app may have several, each with its own hosts, static, pubsub, health and canonical_host)", Example: "{web: {command: bundle exec puma -C config/puma.rb, hosts: [\".myapp.com\"], static: ./public, health: /up, canonical_host: myapp.com}, worker: bundle exec lux jobs:work}", Required: true},
	"autostart": {Block: "app", Name: "Start policy", Description: "start policy: true with the host, false on run/console/any request, button only on a POST to the wake page", Enum: []string{"true", "false", "button"}},
	"deletable": {Block: "app", Name: "Allow destroy", Description: "allow operators to permanently remove this app through the console or dboss destroy"},
	"processes": {Block: "app", Name: "Per-process overrides", Description: "per-process overrides of the process keys, by process name", Example: "{worker: {stop_timeout: 120s}}"},

	// --- Cron ---
	"cron": {Block: "cron", Name: "Scheduled commands", Description: "scheduled one-shot commands by name, run on an every interval or a cron expression", Example: "{cleanup: {schedule: every 6h, command: bundle exec rake cleanup}}"},

	// --- Hooks ---
	"lifecycle": {Block: "lifecycle", Name: "Lifecycle commands", Description: "commands run at a lifecycle step: create once before the first start, start before every start (the processes wait for it), destroy after the app is stopped, before its folder is removed; a command or {command, timeout}, default timeout 3m", Example: "{create: bin/setup-db, start: {command: bin/migrate, timeout: 10m}, destroy: bin/cleanup}"},
	"hooks":     {Block: "hooks", Name: "Deploy hooks", Description: "named one-shot commands triggered by a signed HTTP ping to /hooks/<app>/<hook>; a bare true pulls the current branch and restarts", Example: "{deploy: true}"},

	// --- Deploy ---
	"github_token": {Block: "deploy", Name: "GitHub token", Description: "personal access token a `deploy: true` hook uses to pull a private repo; consumed from the pull process environment only", Example: "$GITHUB_TOKEN", Secret: true},

	// --- Runtime ---
	"idle_stop":           {Block: "runtime", Name: "Idle stop", Description: "stop the app after this long without proxied requests; 0 never", Example: "30m"},
	"health_interval":     {Block: "runtime", Name: "Readiness interval", Description: "poll interval while a web process is starting, until it first answers", Example: "1s"},
	"liveness_interval":   {Block: "runtime", Name: "Liveness interval", Description: "poll interval of the ongoing check once a web process is ready; a hand-run session uses 5m", Example: "30s"},
	"health_timeout":      {Block: "runtime", Name: "Readiness timeout", Description: "give-up time of the readiness check; counts as a failed restart", Example: "90s"},
	"unhealthy_threshold": {Block: "runtime", Name: "Liveness failures", Description: "consecutive liveness_interval failures of a web process before it is restarted; 0 disables the ongoing checks"},
	"stop_timeout":        {Block: "runtime", Name: "Stop timeout", Description: "grace period between stop_signal and SIGKILL", Example: "30s"},
	"stop_signal":         {Block: "runtime", Name: "Stop signal", Description: "signal sent to the process group on stop", Enum: []string{"TERM", "INT", "QUIT", "USR1", "USR2"}},
	"restart":             {Block: "runtime", Name: "Restart policy", Description: "restart policy on exit", Enum: []string{"on-failure", "always", "never"}},
	"max_restarts":        {Block: "runtime", Name: "Max restarts", Description: "consecutive failures before the app is marked crashed", Example: "10"},
	"restart_reset":       {Block: "runtime", Name: "Failure reset", Description: "uptime after which the failure counter resets"},
	"restart_backoff":     {Block: "runtime", Name: "Backoff", Description: "delay between restarts: first delay, multiplier, cap", Example: "[500ms, 2.0, 30s]"},
	"log_max_size":        {Block: "runtime", Name: "Max log size", Description: "rotate a process log file above this size", Example: "50m"},
	"log_keep":            {Block: "runtime", Name: "Kept log files", Description: "rotated log files kept per process; 0 truncates the file in place"},
	"log_tail_lines":      {Block: "runtime", Name: "Tail lines", Description: "lines kept in memory for dboss logs"},
	"log_retention":       {Block: "runtime", Name: "Log retention", Description: "how long request rows and app log files are kept; 0 disables the whole log store for the app", Example: "72h"},
	"stdout_retention":    {Block: "runtime", Name: "Stdout retention", Description: "how long process stdout and the dboss daemon log are kept; 0 disables both"},
	"log_flush":           {Block: "runtime", Name: "Log flush", Description: "request log batch insert interval", Example: "5s"},
	"tmp_clean":           {Block: "runtime", Name: "Tmp cleanup", Description: "delete files under the app's ./tmp older than this once a day; false or 0 never cleans", Example: "7d"},
	"shell":               {Block: "runtime", Name: "Run through shell", Description: "run commands through sh -c instead of exec"},
	"env":                 {Block: "runtime", Name: "Environment", Description: "extra environment for every process, lowest priority", Example: "{RAILS_ENV: production}"},
	"resources":           {Block: "runtime", Name: "Resource backend", Description: "resource backend", Enum: []string{"auto", "cgroup", "procgroup"}},
	"memory_max":          {Block: "runtime", Name: "Memory limit", Description: "memory limit, cgroup backend only; 0 unlimited", Example: "512m"},
	"cpu_max":             {Block: "runtime", Name: "CPU limit", Description: "CPU limit in percent of one core, cgroup backend only; 0 unlimited", Example: "200"},

	// --- Web ---
	"health_endpoint":   {Block: "web", Name: "Health endpoint", Description: "public status path on the app's own hosts: 200 when running or asleep and wakeable, 503 otherwise; empty disables", Example: "/healthz"},
	"static_immutable":  {Block: "web", Name: "Immutable prefixes", Description: "path prefixes under the static directory cached as immutable for a year", Example: "[/assets/, /packs/]"},
	"static_extensions": {Block: "web", Name: "Static extensions", Description: "file extensions served from the static directory, without the dot; empty serves any file", Example: "[css, js, png]"},
	"max_body":          {Block: "web", Name: "Max body size", Description: "request body limit; 0 none"},
	"basic_auth":        {Block: "web", Name: "Basic auth users", Description: "HTTP basic auth users to bcrypt hashes from dboss password", Example: "{alice: \"$2a$10$...\"}", Secret: true},
	"allow_ips":         {Block: "web", Name: "Allowed IPs", Description: "CIDRs allowed to reach the app; empty allows everyone", Example: "[10.0.0.0/8]"},
	"headers":           {Block: "web", Name: "Response headers", Description: "response headers added to every response; an empty value removes one", Example: "{X-Frame-Options: DENY}"},
	"maintenance_page":  {Block: "web", Name: "Maintenance page", Description: "file served in maintenance mode, relative to the app", Example: "./public/503.html"},
	"error_page_path":   {Block: "web", Name: "Error page", Description: "static HTML served for proxy errors and app 5xx answers, relative to the app", Example: "public/error_500.html"},

	// --- Sign-in ---
	"auth.allow_emails": {Block: "auth", Name: "Allowed emails", Description: "emails and *@domain patterns let in through AuthCog, * for any account; empty leaves the app open", Example: "[ana@example.com, \"*@example.com\"]"},
	"auth.session_ttl":  {Block: "auth", Name: "Session lifetime", Description: "how long an app sign-in lasts before AuthCog is asked again", Example: "8h"},

	// --- AuthCog login service ---
	"authcog.login": {Block: "authcog", Name: "Login", Description: "run the AuthCog sign-in for the app and hand it the profile once as X-Dboss-User"},
	"authcog.path":  {Block: "authcog", Name: "Login path", Description: "app URL dboss captures for the sign-in and hands the profile back to", Example: "/authcog"},
	"authcog.realm": {Block: "authcog", Name: "Realm", Description: "AuthCog subdomain, so auth means auth.authcog.com", Example: "auth"},

	// --- Alerts ---
	"alerts.window":       {Block: "alerts", Name: "Window", Description: "sliding window of the request log the checks look at", Example: "10m"},
	"alerts.min_requests": {Block: "alerts", Name: "Minimum requests", Description: "requests the window needs before a check runs, so a quiet app never pages", Example: "50"},
	"alerts.error_rate":   {Block: "alerts", Name: "Error rate", Description: "percent of 5xx answers in the window that posts error-rate; 0 disables", Example: "5"},
	"alerts.slow_p95":     {Block: "alerts", Name: "Slow p95", Description: "p95 request latency in the window that posts slow; 0 disables", Example: "2s"},
}
