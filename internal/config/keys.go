package config

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Key is one documented configuration key: what it is, where it goes and what it holds.
// Path, Type and Default come from the config structs and Default(), so they cannot drift;
// only Description and Example are written by hand in keyDocs.
type Key struct {
	Path        string `json:"path"`
	Group       string `json:"group"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Default     string `json:"default,omitempty"`
	Example     string `json:"example,omitempty"`
	PerProcess  bool   `json:"per_process,omitempty"`
}

// Groups, in display order. Host keys live in the file with apps:, app keys in the file with
// procfile:, shared keys under defaults: in the host file and at the top level of an app file.
const (
	GroupHost   = "host"
	GroupApp    = "app"
	GroupShared = "shared"
)

type keyDoc struct {
	description string
	example     string
	def         string
}

var keyDocs = map[string]keyDoc{
	"apps":                                   {description: "directory of app folders, one entry (folder or symlink) per app", example: "./apps"},
	"state_dir":                              {description: "session state: process state and the console signing key"},
	"log_dir":                                {description: "process logs and per-app request logs"},
	"socket":                                 {description: "unix socket for the control API the CLI talks to"},
	"proxy.listen":                           {description: "one or more addresses to listen on; owns port 80 and routes every request to an app, empty disables the proxy"},
	"proxy.trusted_cidrs":                    {description: "only these source CIDRs may connect; empty accepts anyone", example: "[173.245.48.0/20, 2400:cb00::/32]"},
	"proxy.client_ip_headers":                {description: "headers checked in order for the real client IP"},
	"proxy.wake.retry_after":                 {description: "seconds sent in Retry-After and the starting page refresh"},
	"proxy.wake.starting_page":               {description: "page served while an app is stopped or starting"},
	"proxy.wake.crashed_page":                {description: "page served when an app has crashed"},
	"proxy.wake.unknown_page":                {description: "page served when no app matches the host"},
	"proxy.upstream.dial_timeout":            {description: "connect timeout to an app port"},
	"proxy.upstream.response_header_timeout": {description: "how long an app may take to start responding"},
	"proxy.upstream.idle_conn_timeout":       {description: "idle keep-alive connection lifetime to an app"},
	"proxy.upstream.max_idle_conns_per_app":  {description: "idle keep-alive connections kept per app"},
	"management.host":                        {description: "one or more hostnames of the management console; omit to disable it", example: "boss.example.com"},
	"management.url":                         {description: "public URL of the console as operators open it, printed on start; its host must be one of management.host", example: "https://boss.example.com"},
	"management.auth.realm":                  {description: "AuthCog realm used for sign-in"},
	"management.auth.admin_emails":           {description: "email addresses allowed into the console", example: "[admin@example.com]"},
	"management.auth.session_ttl":            {description: "signed console session lifetime"},
	"management.metrics.enabled":             {description: "serve /healthz, /readyz and /metrics on the management host"},
	"management.metrics.token":               {description: "when set, /metrics requires this as a bearer token; health endpoints stay open", example: "$METRICS_TOKEN"},
	"ports.range":                            {description: "inclusive port range appboss owns; the first port is the console"},
	"daemon.idle_tick":                       {description: "how often idle apps are checked"},
	"daemon.resume_running":                  {description: "on start, resume the apps in running.json (every app on a first start)"},
	"daemon.prune_at":                        {description: "local time of the daily request-log prune"},
	"daemon.log_level":                       {description: "appboss's own log level: debug, info, warn, error"},
	"daemon.log_ingest_interval":             {description: "how often process log segments are sealed and ingested into the log store"},
	"daemon.audit_retention":                 {description: "how long operator audit rows are kept; 0 keeps them forever"},
	"notify.url":                             {description: "webhook that receives crash and failure events; empty disables notifications", example: "$ALERT_WEBHOOK_URL"},
	"notify.format":                          {description: "webhook payload shape: generic, slack, discord or ntfy"},
	"notify.events":                          {description: "events to post: crash, restart-loop, health-timeout, wake-failed, hook-failed"},
	"notify.min_interval":                    {description: "quiet period per app and event, so a crash loop does not spam"},
	"notify.headers":                         {description: "extra headers sent with every webhook request", example: "{Authorization: \"Bearer $TOKEN\"}"},

	"procfile":       {description: "process commands by name; names match [a-z][a-z0-9_-]*", example: "{web: bundle exec puma -C config/puma.rb}"},
	"hosts":          {description: "hostnames routed to the web process; a leading *. matches subdomains, a leading . matches the domain and its subdomains", example: "[\".myapp.com\"]"},
	"web_process":    {description: "process that receives proxied traffic", def: "web"},
	"canonical_host": {description: "301 every other host of this app to this one; must be in hosts", example: "myapp.com"},
	"autostart":      {description: "start this app when the host starts; false waits for run or a request", def: "true"},
	"processes":      {description: "per-process overrides of the process keys, by process name", example: "{worker: {stop_timeout: 120s}}"},
	"cron":           {description: "scheduled one-shot commands by name, run on an every interval or a cron expression", example: "{cleanup: {schedule: every 6h, command: bundle exec rake cleanup}}"},
	"hooks":          {description: "named one-shot commands triggered by a signed HTTP ping to /hooks/<app>/<hook>", example: "{deploy: {command: git pull, restart: true}}"},

	"idle_stop":        {description: "stop the app after this long without proxied requests; 0 never"},
	"health":           {description: "readiness check: tcp, or http:<path> expecting 2xx"},
	"health_interval":  {description: "poll interval of the readiness check"},
	"health_timeout":   {description: "give-up time of the readiness check; counts as a failed restart"},
	"stop_timeout":     {description: "grace period between stop_signal and SIGKILL"},
	"stop_signal":      {description: "signal sent to the process group on stop: TERM, INT, QUIT, USR1, USR2"},
	"restart":          {description: "restart policy on exit: on-failure, always, never"},
	"max_restarts":     {description: "consecutive failures before the app is marked crashed"},
	"restart_reset":    {description: "uptime after which the failure counter resets"},
	"restart_backoff":  {description: "delay between restarts: first delay, multiplier, cap"},
	"log_max_size":     {description: "rotate a process log file above this size"},
	"log_keep":         {description: "rotated log files kept per process"},
	"log_tail_lines":   {description: "lines kept in memory for appboss logs"},
	"log_retention":    {description: "how long request rows and app log files are kept; 0 disables both"},
	"stdout_retention": {description: "how long process stdout and the appboss daemon log are kept; 0 disables both"},
	"log_flush":        {description: "request log batch insert interval"},
	"shell":            {description: "run commands through sh -c instead of exec"},
	"env":              {description: "extra environment for every process, lowest priority", example: "{RAILS_ENV: production}"},
	"resources":        {description: "resource backend: auto, cgroup, procgroup"},
	"memory_max":       {description: "memory limit, cgroup backend only; 0 unlimited"},
	"cpu_max":          {description: "CPU limit in percent of one core, cgroup backend only; 0 unlimited"},
	"static":           {description: "directory served straight from disk for GET and HEAD, relative to the app", example: "./public"},
	"static_immutable": {description: "path prefixes under static cached as immutable for a year"},
	"max_body":         {description: "request body limit; 0 none"},
	"basic_auth":       {description: "HTTP basic auth users to bcrypt hashes from appboss password", example: "{alice: \"$2a$10$...\"}"},
	"allow_ips":        {description: "CIDRs allowed to reach the app; empty allows everyone", example: "[10.0.0.0/8]"},
	"headers":          {description: "response headers added to every response; an empty value removes one", example: "{X-Frame-Options: DENY}"},
	"maintenance_page": {description: "file served in maintenance mode, relative to the app", example: "./public/503.html"},
}

// Keys lists every configuration key in display order: host, app, then shared keys.
func Keys() []Key {
	defaults := Default()
	var keys []Key
	walk(reflect.ValueOf(defaults), "", GroupHost, false, func(key Key) {
		if key.Path != "defaults" && !strings.HasPrefix(key.Path, "defaults.") {
			keys = append(keys, key)
		}
	})
	app := App{WebProcess: "web", Autostart: true}
	walk(reflect.ValueOf(app), "", GroupApp, false, func(key Key) {
		if _, shared := keyDocs[key.Path]; shared && isSharedKey(key.Path) {
			return
		}
		keys = append(keys, key)
	})
	walk(reflect.ValueOf(defaults.Defaults.Process), "", GroupShared, true, func(key Key) { keys = append(keys, key) })
	walk(reflect.ValueOf(defaults.Defaults.Web), "", GroupShared, false, func(key Key) { keys = append(keys, key) })
	return keys
}

// KeyPaths returns every path Keys documents, for the coverage test.
func KeyPaths() []string {
	paths := make([]string, 0, len(keyDocs))
	for path := range keyDocs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func isSharedKey(path string) bool {
	for _, group := range []reflect.Type{reflect.TypeOf(Process{}), reflect.TypeOf(Web{})} {
		for i := 0; i < group.NumField(); i++ {
			if yamlName(group.Field(i)) == path {
				return true
			}
		}
	}
	return false
}

func yamlName(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
	return name
}

// walk visits the yaml-tagged fields of value depth first, expanding inline structs in place
// and nested structs under their key. Leaf values become Keys.
func walk(value reflect.Value, prefix, group string, perProcess bool, visit func(Key)) {
	valueType := value.Type()
	for i := 0; i < valueType.NumField(); i++ {
		field := valueType.Field(i)
		tag := field.Tag.Get("yaml")
		if tag == "" || tag == "-" || !field.IsExported() {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		if strings.Contains(options, "inline") {
			walk(value.Field(i), prefix, group, perProcess, visit)
			continue
		}
		path := prefix + name
		if field.Type.Kind() == reflect.Struct {
			walk(value.Field(i), path+".", group, perProcess, visit)
			continue
		}
		doc := keyDocs[path]
		key := Key{Path: path, Group: group, Type: typeName(field.Type), Description: doc.description, PerProcess: perProcess}
		key.Default = doc.def
		if key.Default == "" {
			key.Default = formatValue(value.Field(i))
		}
		if key.Default == "" {
			key.Example = doc.example
		}
		visit(key)
	}
}

func typeName(t reflect.Type) string {
	switch t {
	case reflect.TypeOf(Duration(0)):
		return "duration"
	case reflect.TypeOf(Size(0)):
		return "size"
	case reflect.TypeOf(List(nil)):
		return "list"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Int:
		return "int"
	case reflect.Bool:
		return "bool"
	case reflect.Slice:
		return "list"
	case reflect.Array:
		return "[from, to]"
	case reflect.Map:
		return "map"
	}
	return t.Kind().String()
}

// formatValue renders a default the way it is written in appboss.yaml. Empty strings, lists
// and maps render as "" so the caller falls back to the example.
func formatValue(value reflect.Value) string {
	switch v := value.Interface().(type) {
	case Duration:
		return formatDuration(v.Value())
	case Size:
		return v.String()
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case bool:
		return strconv.FormatBool(v)
	case [2]int:
		return fmt.Sprintf("[%d, %d]", v[0], v[1])
	case List:
		return formatValue(reflect.ValueOf([]string(v)))
	case []string:
		if len(v) == 0 {
			return ""
		}
		return "[" + strings.Join(v, ", ") + "]"
	case []any:
		if len(v) == 0 {
			return ""
		}
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = fmt.Sprint(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]string:
		if len(v) == 0 {
			return ""
		}
		names := slices.Sorted(maps.Keys(v))
		parts := make([]string, len(names))
		for i, name := range names {
			parts[i] = name + ": " + v[name]
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	if value.Kind() == reflect.Map && value.Len() == 0 {
		return ""
	}
	return fmt.Sprint(value.Interface())
}

func formatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}
