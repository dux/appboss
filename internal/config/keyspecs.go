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
	// --- Host ---
	"apps":            {Block: "host", Name: "Apps directory", Description: "directory of app folders, one entry (folder or symlink) per app", Example: "./apps"},
	"dir":             {Block: "host", Name: "Runtime directory", Description: "everything dboss writes: state/, log/ and the dboss.sock control socket", Example: "/var/lib/dboss"},
	"ports":           {Block: "host", Name: "Port range", Description: "inclusive port range dboss owns; the first port is the console"},
	"log_level":       {Block: "host", Name: "Log level", Description: "dboss's own log level", Enum: []string{"debug", "info", "warn", "error"}},
	"audit_retention": {Block: "host", Name: "Audit retention", Description: "how long operator audit rows are kept; 0 keeps them forever", Example: "30d"},
	"maintenance_at":  {Block: "host", Name: "Maintenance time", Description: "local time of the daily log prune, followed by the SQLite VACUUM", Example: "03:30"},
	"authcog_realm":   {Block: "host", Name: "AuthCog realm", Description: "AuthCog host the console and every app sign-in use", Example: "dboss.authcog.com"},

	// --- Tokens ---
	"tokens.github": {Block: "tokens", Name: "GitHub token", Description: "outbound: personal access token a pull hook and a github_pr preview use for a private repo; consumed from the process environment only", Example: "$GITHUB_TOKEN", Secret: true},
	"tokens.dboss":  {Block: "tokens", Name: "Dboss token", Description: "inbound: every /hooks ping and /metrics must present it; unset refuses hooks and hides /metrics", Example: "$DBOSS_TOKEN", Secret: true},

	// --- Proxy ---
	"proxy.listen":     {Block: "proxy", Name: "Listen addresses", Description: "one or more addresses to listen on; owns port 80 and routes every request to an app, empty disables the proxy", Example: "127.0.0.1:8080"},
	"proxy.cloudflare": {Block: "proxy", Name: "Behind Cloudflare", Description: "accept connections only from Cloudflare's published ranges (and loopback) and read the client address from CF-Connecting-IP"},
	"proxy.tls.listen": {Block: "proxy", Name: "TLS listener", Description: "address for the built-in HTTPS listener with Let's Encrypt certificates; empty means TLS terminates elsewhere (Cloudflare)", Example: ":443"},
	"proxy.tls.email":  {Block: "proxy", Name: "ACME email", Description: "contact email registered with the certificate authority", Example: "ops@example.com"},
	"proxy.timeout":    {Block: "proxy", Name: "Upstream timeout", Description: "how long an app may take to start responding", Example: "5m"},

	// --- Management console ---
	"management.host":   {Block: "management", Name: "Console hosts", Description: "one or more hostnames of the management console, served at https://<first host>; omit to disable it", Example: "dboss.example.com"},
	"management.admins": {Block: "management", Name: "Admins", Description: "email addresses allowed into the console", Example: "[admin@example.com]"},

	// --- Notifications ---
	"notify.url":     {Block: "notify", Name: "Webhook URL", Description: "webhook that receives crash and failure events; Slack, Discord and ntfy URLs get their own payload shape; empty disables notifications", Example: "$ALERT_WEBHOOK_URL"},
	"notify.events":  {Block: "notify", Name: "Events", Description: "events to post: crash, restart-loop, health-timeout, wake-failed, hook-failed, deploy, config-changed, backup-failed, error-rate, slow"},
	"notify.headers": {Block: "notify", Name: "Extra headers", Description: "extra headers sent with every webhook request", Example: "{Authorization: \"Bearer $TOKEN\"}"},

	// --- PostgreSQL ---
	"postgres.dsn":     {Block: "postgres", Name: "Connection string", Description: "libpq connection string or URL; empty auto-detects the local socket then 127.0.0.1 using the PG* environment; `postgres: false` turns the feature off", Example: "$DATABASE_URL", Secret: true},
	"postgres.backups": {Block: "postgres", Name: "Backups", Description: "databases dumped by the daily run, each with its rotation: week or month", Example: "{myapp_production: week, reports: month}"},

	// --- App ---
	"procfile":  {Block: "app", Name: "Process commands", Description: "process commands by name; names match [a-z][a-z0-9_-]*. A scalar is a background process; every mapping that adds hosts is a web process (an app may have several, each with its own hosts, static, pubsub, health and canonical_host)", Example: "{web: {command: bundle exec puma -C config/puma.rb, hosts: [\".myapp.com\"], static: ./public, health: /up, canonical_host: myapp.com}, worker: bundle exec lux jobs:work}", Required: true},
	"autostart": {Block: "app", Name: "Start policy", Description: "start policy: true with the host, false on run/console/any request, button only on a POST to the wake page", Enum: []string{"true", "false", "button"}},
	"deletable": {Block: "app", Name: "Allow destroy", Description: "allow operators to permanently remove this app through the console or dboss destroy"},
	"pages":     {Block: "app", Name: "Pages", Description: "folder of the dboss pages served for the app (<name>.html, else template.html, else the host's, else built in); in the host file the fallback for every app", Example: "./public/errors"},
	"processes": {Block: "app", Name: "Per-process overrides", Description: "per-process overrides of the process keys, by process name", Example: "{worker: {stop_timeout: 120s}}"},

	// --- Cron ---
	"cron": {Block: "cron", Name: "Scheduled commands", Description: "scheduled one-shot commands by name, run on an every interval or a cron expression", Example: "{cleanup: {schedule: every 6h, command: bundle exec rake cleanup}}"},

	// --- Hooks ---
	"lifecycle": {Block: "lifecycle", Name: "Lifecycle commands", Description: "commands run at a lifecycle step: create once before the first start, start before every start (the processes wait for it), destroy after the app is stopped, before its folder is removed; a command or {command, timeout}, default timeout 3m", Example: "{create: bin/setup-db, start: {command: bin/migrate, timeout: 10m}, destroy: bin/cleanup}"},
	"hooks":     {Block: "hooks", Name: "Deploy hooks", Description: "named one-shot commands triggered by a ping to /hooks/<app>/<hook> signed with tokens.dboss; a bare true pulls the current branch and restarts", Example: "{deploy: true}"},

	// --- Runtime ---
	"idle_stop":           {Block: "runtime", Name: "Idle stop", Description: "stop the app after this long without proxied requests; 0 never", Example: "30m"},
	"liveness_interval":   {Block: "runtime", Name: "Liveness interval", Description: "poll interval of the ongoing check once a web process is ready; a hand-run session uses 5m", Example: "30s"},
	"health_timeout":      {Block: "runtime", Name: "Readiness timeout", Description: "give-up time of the readiness check; counts as a failed restart", Example: "90s"},
	"unhealthy_threshold": {Block: "runtime", Name: "Liveness failures", Description: "consecutive liveness_interval failures of a web process before it is restarted; 0 disables the ongoing checks"},
	"stop_timeout":        {Block: "runtime", Name: "Stop timeout", Description: "grace period between stop_signal and SIGKILL", Example: "30s"},
	"stop_signal":         {Block: "runtime", Name: "Stop signal", Description: "signal sent to the process group on stop", Enum: []string{"TERM", "INT", "QUIT", "USR1", "USR2"}},
	"restart":             {Block: "runtime", Name: "Restart policy", Description: "restart policy on exit", Enum: []string{"on-failure", "always", "never"}},
	"max_restarts":        {Block: "runtime", Name: "Max restarts", Description: "consecutive failures before the app is marked crashed", Example: "10"},
	"log_retention":       {Block: "runtime", Name: "Log retention", Description: "how long request rows and app log files are kept; 0 disables the whole log store for the app", Example: "72h"},
	"stdout_retention":    {Block: "runtime", Name: "Stdout retention", Description: "how long process stdout and the dboss daemon log are kept; 0 disables both"},
	"tmp_clean":           {Block: "runtime", Name: "Tmp cleanup", Description: "delete files under the app's ./tmp older than this once a day; false or 0 never cleans", Example: "7d"},
	"env":                 {Block: "runtime", Name: "Environment", Description: "extra environment for every process, lowest priority", Example: "{RAILS_ENV: production}"},
	"memory_max":          {Block: "runtime", Name: "Memory limit", Description: "memory limit, cgroup v2 hosts only; 0 unlimited", Example: "512m"},
	"cpu_max":             {Block: "runtime", Name: "CPU limit", Description: "CPU limit in percent of one core, cgroup v2 hosts only; 0 unlimited", Example: "200"},

	// --- Web ---
	"health_endpoint":   {Block: "web", Name: "Health endpoint", Description: "public status path on the app's own hosts: 200 when running or asleep and wakeable, 503 otherwise; empty disables", Example: "/healthz"},
	"static_immutable":  {Block: "web", Name: "Immutable prefixes", Description: "path prefixes under the static directory cached as immutable for a year", Example: "[/assets/, /packs/]"},
	"static_extensions": {Block: "web", Name: "Static extensions", Description: "file extensions served from the static directory, without the dot; empty serves any file", Example: "[css, js, png]"},
	"max_body":          {Block: "web", Name: "Max body size", Description: "request body limit; 0 none"},
	"basic_auth":        {Block: "web", Name: "Basic auth users", Description: "HTTP basic auth users to bcrypt hashes from dboss password", Example: "{alice: \"$2a$10$...\"}", Secret: true},
	"allow_ips":         {Block: "web", Name: "Allowed IPs", Description: "CIDRs allowed to reach the app; empty allows everyone", Example: "[10.0.0.0/8]"},
	"headers":           {Block: "web", Name: "Response headers", Description: "response headers added to every response; an empty value removes one", Example: "{X-Frame-Options: DENY}"},

	// --- Sign-in ---
	"auth":        {Block: "auth", Name: "Allowed emails", Description: "emails and *@domain patterns let in through AuthCog, * for any account; empty leaves the app open", Example: "[ana@example.com, \"*@example.com\"]"},
	"session_ttl": {Block: "auth", Name: "Session lifetime", Description: "how long a sign-in lasts before AuthCog is asked again; the host value also covers the console", Example: "8h"},
	"authcog":     {Block: "auth", Name: "AuthCog login", Description: "run the AuthCog sign-in for the app at this path (true means /authcog) and hand it the profile once as X-Dboss-User", Example: "/login"},

	// --- Alerts ---
	"alerts.error_rate": {Block: "alerts", Name: "Error rate", Description: "percent of 5xx answers over the last 5 minutes that posts error-rate; 0 disables", Example: "5"},
	"alerts.slow_p95":   {Block: "alerts", Name: "Slow p95", Description: "p95 request latency over the last 5 minutes that posts slow; 0 disables", Example: "2s"},
}
