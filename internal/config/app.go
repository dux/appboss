package config

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPubsubPath is the path a bare `pubsub: true` enables on the web process.
const DefaultPubsubPath = "/socketio"

// DefaultStatic is the directory a web process serves static files from unless it sets `static`.
const DefaultStatic = "./public"

// DefaultPages is the folder, relative to the app (or the host), holding the HTML pages dboss
// serves for it: <name>.html per page, template.html for every page.
const DefaultPages = "./public/error_pages"

// DevHost is bound to a single-app config that declares no hosts, so `dboss start` inside an
// app folder serves it on *.lvh.me with no config.
const DevHost = ".lvh.me"

// ProcessSpec is one entry of a procfile: the command to run, the hosts the single web process
// answers, its optional realtime hub, its readiness check and the canonical hostname. A scalar
// value is the command alone, so a background process stays a one-liner; a mapping adds the rest.
type ProcessSpec struct {
	Command string      `yaml:"command" json:"command"`
	Hosts   List        `yaml:"hosts,omitempty" json:"hosts,omitempty"`
	Pubsub  *PubsubSpec `yaml:"pubsub,omitempty" json:"pubsub,omitempty"`
	// Health is the web process's readiness path, e.g. /up; empty means a TCP connect.
	Health string `yaml:"health,omitempty" json:"health,omitempty"`
	// CanonicalHost is the web process hostname every other host redirects to; it must be one
	// of Hosts.
	CanonicalHost string `yaml:"canonical_host,omitempty" json:"canonical_host,omitempty"`
	// Count is how many copies of the process run; 0 means one. Copies of a web process share
	// its hosts and the proxy balances between them.
	Count int `yaml:"count,omitempty" json:"count,omitempty"`
}

// MaxCount bounds procfile count, so one typo cannot claim the whole port range.
const MaxCount = 64

// Instances is how many copies of the process run.
func (p ProcessSpec) Instances() int { return max(p.Count, 1) }

// processSpecKeys are the keys of the mapping form, in the order the hints name them. They are
// checked here because a custom decoder is a leaf as far as the schema walk is concerned.
var processSpecKeys = []string{"command", "hosts", "pubsub", "health", "canonical_host", "count"}

// processSpecFields is ProcessSpec without its methods, so the mapping form decodes and encodes
// through the struct tags instead of recursing into the custom marshalers.
type processSpecFields ProcessSpec

// UnmarshalYAML accepts a bare command string or the processSpecKeys mapping.
func (p *ProcessSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*p = ProcessSpec{Command: node.Value}
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if key := node.Content[i].Value; !slices.Contains(processSpecKeys, key) {
				return &Error{Line: node.Content[i].Line, Key: "procfile", Message: fmt.Sprintf("unknown key %q", key), Hint: "valid keys here: " + strings.Join(processSpecKeys, ", ")}
			}
		}
		return node.Decode((*processSpecFields)(p))
	}
	return &Error{Line: node.Line, Key: "procfile", Message: "must be a command or a {" + strings.Join(processSpecKeys, ", ") + "} mapping"}
}

// commandOnly reports whether the process only runs a command, which marshals as a scalar.
func (p ProcessSpec) commandOnly() bool {
	return len(p.Hosts) == 0 && p.Pubsub == nil && p.Health == "" && p.CanonicalHost == "" && p.Count <= 1
}

// MarshalYAML writes a scalar command when the process only runs a command, else the full mapping,
// so a resolved config reads like the file it came from.
func (p ProcessSpec) MarshalYAML() (any, error) {
	if p.commandOnly() {
		return p.Command, nil
	}
	return processSpecFields(p), nil
}

// MarshalJSON mirrors MarshalYAML for `dboss config -d --json`.
func (p ProcessSpec) MarshalJSON() ([]byte, error) {
	if p.commandOnly() {
		return json.Marshal(p.Command)
	}
	return json.Marshal(processSpecFields(p))
}

// StaticPath is the app-level static file option: a directory served straight from disk, true for
// the default ./public, or false to disable. An absent option uses the default.
type StaticPath string

// UnmarshalYAML accepts true (the default directory), a path, or false (disabled).
func (s *StaticPath) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return &Error{Line: node.Line, Key: "static", Message: "must be true, a path, or false"}
	}
	switch node.Tag {
	case "!!bool":
		if node.Value == "true" {
			*s = DefaultStatic
		}
		return nil
	case "!!null":
		return nil
	}
	*s = StaticPath(node.Value)
	return nil
}

// MarshalYAML writes false when static serving is off, else the directory.
func (s StaticPath) MarshalYAML() (any, error) {
	if s == "" {
		return false, nil
	}
	return string(s), nil
}

func (s StaticPath) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte("false"), nil
	}
	return json.Marshal(string(s))
}

// UnmarshalJSON mirrors UnmarshalYAML for the console's JSON config payloads.
func (s *StaticPath) UnmarshalJSON(data []byte) error {
	var path string
	if err := json.Unmarshal(data, &path); err == nil {
		*s = StaticPath(path)
		return nil
	}
	var enabled bool
	if err := json.Unmarshal(data, &enabled); err != nil {
		return err
	}
	if enabled {
		*s = DefaultStatic
		return nil
	}
	*s = ""
	return nil
}

// PubsubSpec is the web process's realtime option: `true` for the default path, a bare path, or a
// mapping of the full pubsub settings. An empty or absent option disables the hub, and an empty
// path disables it too, matching the path-as-switch rule.
type PubsubSpec struct {
	Path     string
	Settings *PubsubOverrides
}

func (p PubsubSpec) enabled() bool { return p.Path != "" }

// UnmarshalYAML accepts true, a path string, or a {path, secret, replay, ...} mapping.
func (p *PubsubSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!bool":
			if node.Value == "true" {
				p.Path = DefaultPubsubPath
			}
			return nil
		case "!!null":
			return nil
		}
		p.Path = node.Value
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if key := node.Content[i].Value; !validPubsubKey(key) {
				return &Error{Line: node.Content[i].Line, Key: "pubsub", Message: fmt.Sprintf("unknown key %q", key), Hint: "valid keys here: path, secret, replay, max_clients, max_message_size, client_events, test"}
			}
		}
		var settings PubsubOverrides
		if err := node.Decode(&settings); err != nil {
			return err
		}
		p.Settings = &settings
		if settings.Path != nil {
			p.Path = *settings.Path
		}
		return nil
	}
	return &Error{Line: node.Line, Key: "pubsub", Message: "must be true, a path, or a mapping of pubsub settings"}
}

// MarshalYAML writes the mapping form when the full block was given, else the path shorthand.
func (p PubsubSpec) MarshalYAML() (any, error) {
	if p.Settings != nil {
		return p.Settings, nil
	}
	return p.Path, nil
}

func (p PubsubSpec) MarshalJSON() ([]byte, error) {
	if p.Settings != nil {
		return json.Marshal(p.Settings)
	}
	return json.Marshal(p.Path)
}

func validPubsubKey(key string) bool {
	switch key {
	case "path", "secret", "replay", "max_clients", "max_message_size", "client_events", "test":
		return true
	}
	return false
}

// WebProcess is one procfile entry that answers proxied traffic: the hosts it serves, the
// canonical hostname its other hosts redirect to, and its realtime hub. An app may have several,
// each bound to its own PORT.
type WebProcess struct {
	Name          string
	Hosts         List
	CanonicalHost string
	Pubsub        Pubsub
	// Static is the directory served straight from disk; empty disables it.
	Static string
}

type App struct {
	Procfile map[string]ProcessSpec `yaml:"procfile" json:"procfile"`
	// WebProcesses and Hosts are derived from every procfile entry that declares hosts; they
	// are never written back to YAML and exist for the supervisor and proxy.
	WebProcesses []WebProcess       `yaml:"-" json:"-"`
	Hosts        List               `yaml:"-" json:"-"`
	Autostart    Autostart          `yaml:"autostart" json:"autostart"`
	Deletable    bool               `yaml:"deletable" json:"deletable"`
	Cron         map[string]CronJob `yaml:"cron" json:"cron"`
	Hooks        map[string]Hook    `yaml:"hooks" json:"hooks"`
	// Lifecycle maps a step (create, start, destroy) to the command dboss runs at that point.
	Lifecycle map[string]LifecycleCommand `yaml:"lifecycle" json:"lifecycle,omitempty"`
	// Pages is the folder of the app's own dboss pages, relative to the app.
	Pages     string `yaml:"pages" json:"pages"`
	Defaults  `yaml:",inline"`
	Processes map[string]ProcessOverrides `yaml:"processes" json:"processes"`
}

// processNames lists procfile names sorted, so every derived choice and error is deterministic.
func processNames(procfile map[string]ProcessSpec) []string {
	names := make([]string, 0, len(procfile))
	for name := range procfile {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsWeb reports whether a procfile entry serves proxied traffic because it declares hosts.
func (a App) IsWeb(name string) bool {
	for _, web := range a.WebProcesses {
		if web.Name == name {
			return true
		}
	}
	return false
}

// deriveWeb builds one WebProcess per procfile entry that declares hosts, unions their hosts and
// rejects a host pattern used by two processes of the same app.
func (a *App) deriveWeb() error {
	seen := map[string]string{}
	for _, name := range processNames(a.Procfile) {
		spec := a.Procfile[name]
		if !jobName.MatchString(name) {
			return keyErr("procfile", "invalid process name %q", name)
		}
		if strings.TrimSpace(spec.Command) == "" {
			return keyErr("procfile."+name, "command is empty")
		}
		if spec.Count < 0 || spec.Count > MaxCount {
			return keyErr("procfile."+name+".count", "must be between 1 and %d", MaxCount)
		}
		if len(spec.Hosts) == 0 {
			continue
		}
		web := WebProcess{Name: name, Hosts: spec.Hosts, CanonicalHost: spec.CanonicalHost}
		a.WebProcesses = append(a.WebProcesses, web)
		for _, host := range spec.Hosts {
			if !validHostPattern(host) {
				return keyErr("procfile."+name+".hosts", "invalid host pattern %q", host)
			}
			normalized := NormalizePattern(host)
			if owner, ok := seen[normalized]; ok {
				return &Error{Key: "procfile." + name + ".hosts", Message: fmt.Sprintf("host pattern %q is already used by process %q", host, owner)}
			}
			seen[normalized] = name
			a.Hosts = append(a.Hosts, host)
		}
	}
	return nil
}

// resolveStatic copies the app-level static directory onto every web process, so the option lives
// in one place. An empty value disables serving.
func (a *App) resolveStatic() {
	for index := range a.WebProcesses {
		a.WebProcesses[index].Static = string(a.Web.Static)
	}
}

// resolveHealth moves each web process's health path onto its process keys. Only a web process may
// set it, and the value is a path because dboss always talks HTTP to a process port.
func (a *App) resolveHealth() error {
	for _, name := range processNames(a.Procfile) {
		spec := a.Procfile[name]
		if spec.Health == "" {
			continue
		}
		if !a.IsWeb(name) {
			return &Error{Key: "procfile." + name + ".health", Message: "health is only valid on a web process", Hint: "declare hosts on this process, or set health on a process that declares them"}
		}
		if !strings.HasPrefix(spec.Health, "/") {
			return &Error{Key: "procfile." + name + ".health", Message: fmt.Sprintf("must be a path starting with /, not %q", spec.Health), Hint: "e.g. health: /up"}
		}
		override := a.Processes[name]
		path := spec.Health
		override.Health = &path
		a.Processes[name] = override
	}
	return nil
}

// resolvePubsub resolves every web process's pubsub option onto its WebProcess. A process that
// sets pubsub must declare hosts so the hub has hosts to serve on.
func (a *App) resolvePubsub() error {
	for index := range a.WebProcesses {
		web := &a.WebProcesses[index]
		spec := a.Procfile[web.Name]
		if spec.Pubsub == nil || !spec.Pubsub.enabled() {
			continue
		}
		pubsub := defaultPubsub
		if spec.Pubsub.Path != "" {
			pubsub.Path = spec.Pubsub.Path
		}
		if settings := spec.Pubsub.Settings; settings != nil {
			apply(&pubsub, *settings)
		}
		if err := validatePubsub(pubsub, "procfile."+web.Name+".pubsub"); err != nil {
			return err
		}
		web.Pubsub = pubsub
	}
	for _, name := range processNames(a.Procfile) {
		spec := a.Procfile[name]
		if spec.Pubsub != nil && spec.Pubsub.enabled() && !a.IsWeb(name) {
			return &Error{Key: "procfile." + name + ".pubsub", Message: "pubsub needs hosts on the same process", Hint: "add hosts to this process or move pubsub to a web process"}
		}
	}
	return nil
}

// resolveCanonical validates each web process's canonical_host against its own hosts and rejects
// canonical_host on a process that serves no hosts.
func (a *App) resolveCanonical() error {
	for _, web := range a.WebProcesses {
		if web.CanonicalHost != "" && !hostAllowed(web.CanonicalHost, web.Hosts) {
			return keyErr("procfile."+web.Name+".canonical_host", "%q is not one of the hosts %v", web.CanonicalHost, web.Hosts)
		}
	}
	for _, name := range processNames(a.Procfile) {
		spec := a.Procfile[name]
		if spec.CanonicalHost != "" && !a.IsWeb(name) {
			return &Error{Key: "procfile." + name + ".canonical_host", Message: "canonical_host is only valid on a web process", Hint: "declare hosts on this process, or set canonical_host on a process that declares them"}
		}
	}
	return nil
}

// UseDevHosts binds the first process to DevHost when no process declares hosts. It is
// single-app mode only, so a host running a folder with no config still gets a hostname.
func (a *App) UseDevHosts() {
	if len(a.Hosts) > 0 || len(a.Procfile) == 0 {
		return
	}
	name := "web"
	if _, ok := a.Procfile["web"]; !ok {
		name = processNames(a.Procfile)[0]
	}
	a.WebProcesses = []WebProcess{{Name: name, Hosts: List{DevHost}, Static: string(a.Web.Static)}}
	a.Hosts = List{DevHost}
}

type appFile struct {
	Procfile  map[string]ProcessSpec      `yaml:"procfile"`
	Autostart Autostart                   `yaml:"autostart"`
	Deletable bool                        `yaml:"deletable"`
	Cron      map[string]CronJob          `yaml:"cron"`
	Hooks     map[string]Hook             `yaml:"hooks"`
	Lifecycle map[string]LifecycleCommand `yaml:"lifecycle"`
	Pages     string                      `yaml:"pages"`
	Overrides `yaml:",inline"`
	Processes map[string]ProcessOverrides `yaml:"processes"`
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
	keys, root, err := decode(data, path, &raw, false)
	if err != nil {
		return App{}, err
	}
	for _, key := range hostKeys {
		if keys[key] {
			return App{}, located(keyErr(key, "is only valid in the root %s", FileName), path, root)
		}
	}
	app, err := buildApp(raw.appFile, defaults, false)
	if err != nil {
		return App{}, located(err, path, root)
	}
	return app, nil
}

func buildApp(raw appFile, defaults Defaults, dev bool) (App, error) {
	if len(raw.Procfile) == 0 {
		return App{}, &Error{Key: "procfile", Message: "must contain at least one process", Hint: "e.g. procfile:\n    web: bundle exec puma"}
	}
	app := App{Procfile: raw.Procfile, Autostart: AutostartOn, Deletable: raw.Deletable, Cron: raw.Cron, Hooks: raw.Hooks, Lifecycle: raw.Lifecycle, Pages: DefaultPages, Defaults: defaults, Processes: raw.Processes}
	if raw.Pages != "" {
		app.Pages = raw.Pages
	}
	if err := app.deriveWeb(); err != nil {
		return App{}, err
	}
	if raw.Autostart != "" {
		app.Autostart = raw.Autostart
	}
	for name := range app.Processes {
		if _, ok := app.Procfile[name]; !ok {
			return App{}, keyErr("processes."+name, "is not in procfile")
		}
	}
	if app.Processes == nil {
		app.Processes = map[string]ProcessOverrides{}
	}
	apply(&app.Defaults, raw.Overrides)
	if dev {
		app.UseDevHosts()
	}
	app.resolveStatic()
	if err := app.resolveHealth(); err != nil {
		return App{}, err
	}
	if err := app.resolvePubsub(); err != nil {
		return App{}, err
	}
	if err := app.resolveCanonical(); err != nil {
		return App{}, err
	}
	for _, web := range app.WebProcesses {
		if web.Pubsub.Enabled() && app.AuthCog.Enabled() && web.Pubsub.Path == string(app.AuthCog) {
			return App{}, &Error{Key: "procfile." + web.Name + ".pubsub", Message: fmt.Sprintf("path %q collides with authcog", web.Pubsub.Path)}
		}
	}
	if err := validateDefaults(app.Defaults); err != nil {
		return App{}, err
	}
	if err := validateCron(app.Cron); err != nil {
		return App{}, err
	}
	if err := validateHooks(app.Hooks); err != nil {
		return App{}, err
	}
	if err := validateLifecycle(app.Lifecycle); err != nil {
		return App{}, err
	}
	app.allowPrefixes, _ = parsePrefixes(app.AllowIPs)
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
