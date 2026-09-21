package super

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/hook"
	"dboss/internal/logx"
	"dboss/internal/notify"
	"dboss/internal/ports"
	"dboss/internal/res"
)

type State string

const (
	Stopped  State = "stopped"
	Starting State = "starting"
	Running  State = "running"
	Stopping State = "stopping"
	Crashed  State = "crashed"
)

type ProcessSnapshot struct {
	Name        string    `json:"name"`
	Command     string    `json:"command"`
	State       State     `json:"state"`
	PID         int       `json:"pid,omitempty"`
	Port        int       `json:"port"`
	Restarts    int       `json:"restarts"`
	MemoryBytes int64     `json:"memory_bytes,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
}

type RequestRates struct {
	LastMinute int64 `json:"last_minute"`
	LastHour   int64 `json:"last_hour"`
	LastDay    int64 `json:"last_day"`
}

// WebProcessSnapshot is one web process of an app: the hosts it answers and the canonical host
// its other hosts redirect to, plus its realtime hub.
type WebProcessSnapshot struct {
	Name          string        `json:"name"`
	Hosts         []string      `json:"hosts"`
	CanonicalHost string        `json:"canonical_host,omitempty"`
	Static        string        `json:"static"`
	Pubsub        config.Pubsub `json:"pubsub"`
}

// ExecResult is the combined output and exit code of a one-off command.
type ExecResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

type Snapshot struct {
	Name            string               `json:"name"`
	State           State                `json:"state"`
	Maintenance     bool                 `json:"maintenance"`
	Draining        bool                 `json:"draining,omitempty"`
	Dir             string               `json:"dir"`
	Hosts           []string             `json:"hosts"`
	WebProcesses    []WebProcessSnapshot `json:"web_processes"`
	Autostart       bool                 `json:"autostart"`
	Deletable       bool                 `json:"deletable"`
	WakeButton      bool                 `json:"wake_button,omitempty"`
	Web             config.Web           `json:"web"`
	Processes       []ProcessSnapshot    `json:"processes"`
	Cron            []CronSnapshot       `json:"cron,omitempty"`
	Hooks           []HookSnapshot       `json:"hooks,omitempty"`
	LastActivity    time.Time            `json:"last_activity,omitempty"`
	Uptime          string               `json:"uptime,omitempty"`
	Resources       res.Stats            `json:"resources"`
	RequestRates    RequestRates         `json:"request_rates"`
	Error           string               `json:"error,omitempty"`
	ErrorLog        []string             `json:"error_log,omitempty"`
	LogRetention    time.Duration        `json:"-"`
	StdoutRetention time.Duration        `json:"-"`
	LogFlush        time.Duration        `json:"-"`
}

// Serving reports whether a request for the app would be answered by the app itself: it runs, or
// it is stopped and the proxy wakes it on the next request. Button apps only wake on a POST.
func (s Snapshot) Serving() bool {
	if s.Draining || s.Maintenance {
		return false
	}
	return s.State == Running || (s.State == Stopped && !s.WakeButton)
}

// WebForHost returns the web process whose hosts best match host. A request that resolved to an
// app is always served by exactly one of its web processes, and the longest pattern wins.
func (s Snapshot) WebForHost(host string) (WebProcessSnapshot, bool) {
	host = config.NormalizeHost(host)
	bestScore := -1
	var best WebProcessSnapshot
	for _, web := range s.WebProcesses {
		for _, pattern := range web.Hosts {
			score, match := config.MatchHost(host, strings.ToLower(pattern))
			if match && score > bestScore {
				best, bestScore = web, score
			}
		}
	}
	return best, bestScore >= 0
}

// WebProcessSnapshots converts the derived config web processes into the snapshot shape.
func WebProcessSnapshots(webs []config.WebProcess) []WebProcessSnapshot {
	result := make([]WebProcessSnapshot, 0, len(webs))
	for _, web := range webs {
		result = append(result, WebProcessSnapshot{Name: web.Name, Hosts: web.Hosts, CanonicalHost: web.CanonicalHost, Static: web.Static, Pubsub: web.Pubsub})
	}
	return result
}

const failureLogLines = 1000

// Databases turns an app's pg_db block into the connection URLs its processes are spawned with,
// creating a database that is missing. It is the supervisor's only view of PostgreSQL; a nil
// value disables the feature, and an app that asks for databases then fails to start.
type Databases interface {
	AppDatabases(ctx context.Context, app string, databases map[string]string) (map[string]string, error)
}

type Manager struct {
	cfg             config.Config
	hostConfig      config.Config
	ports           *ports.Allocator
	backend         res.Backend
	cgroup          res.Backend
	echo            *Echo
	secrets         *hook.Store
	databases       Databases
	sink            notify.Sink
	closeOnce       sync.Once
	rescanMu        sync.Mutex
	mu              sync.RWMutex
	apps            map[string]*appRuntime
	restartRequired []string
	desiredMu       sync.Mutex
	desired         map[string]bool
	maintenance     map[string]bool
	activities      map[string]time.Time
	inflightMu      sync.Mutex
	inflight        map[string]*atomic.Int64
	ctx             context.Context
	cancel          context.CancelFunc
}

// New discovers the apps and starts the ones that were running before, skipping autostart: false.
// A non-nil echo mirrors every process's output to it, which the foreground session uses when
// attached to a terminal. A non-nil databases resolves pg_db blocks. An optional sink receives
// crash and failure events.
func New(cfg config.Config, allocator *ports.Allocator, echo *Echo, databases Databases, sinks ...notify.Sink) (*Manager, []error, error) {
	discovered, invalid, err := apps.Discover(cfg)
	if err != nil {
		return nil, invalid, err
	}
	runningPath := filepath.Join(cfg.StateDir, "running.json")
	desired, err := loadNames(runningPath)
	if err != nil {
		return nil, invalid, err
	}
	// A host with no running list yet starts every autostart app. Once the list exists it is
	// authoritative, so an app someone stopped stays stopped across restarts. autostart: false
	// apps are not started here even when listed; run, the console, or a request starts them.
	if _, statErr := os.Stat(runningPath); errors.Is(statErr, os.ErrNotExist) {
		for _, spec := range discovered {
			if spec.Config.Autostart.Starts() {
				desired[spec.Name] = true
			}
		}
	}
	maintenance, err := loadNames(filepath.Join(cfg.StateDir, "maintenance.json"))
	if err != nil {
		return nil, invalid, err
	}
	activities, err := loadActivities(cfg.StateDir)
	if err != nil {
		return nil, invalid, err
	}
	secrets, err := hook.Open(cfg.StateDir)
	if err != nil {
		return nil, invalid, err
	}
	var sink notify.Sink
	if len(sinks) > 0 {
		sink = sinks[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{cfg: cfg, hostConfig: cfg, ports: allocator, backend: res.Procgroup{}, cgroup: selectCgroup(cfg), echo: echo, secrets: secrets, databases: databases, sink: sink, apps: map[string]*appRuntime{}, desired: desired, maintenance: maintenance, activities: activities, inflight: map[string]*atomic.Int64{}, ctx: ctx, cancel: cancel}
	for _, spec := range discovered {
		if err := m.assignPorts(spec); err != nil {
			cancel()
			return nil, invalid, err
		}
		m.add(ctx, spec)
	}
	m.syncHookSecrets(discovered)
	if cfg.Daemon.ResumeRunning {
		for name := range desired {
			if runtime := m.apps[name]; runtime != nil && runtime.spec.Config.Autostart.Starts() {
				_ = runtime.call(request{kind: requestStart})
			}
		}
	}
	go m.idleLoop(ctx)
	go m.cronLoop(ctx)
	return m, invalid, nil
}

func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		_ = m.saveActivities()
		m.mu.RLock()
		runtimes := make([]*appRuntime, 0, len(m.apps))
		for _, runtime := range m.apps {
			runtimes = append(runtimes, runtime)
		}
		m.mu.RUnlock()
		var wait sync.WaitGroup
		for _, runtime := range runtimes {
			wait.Add(1)
			go func() {
				defer wait.Done()
				_ = runtime.call(request{kind: requestStop})
			}()
		}
		wait.Wait()
		m.cancel()
		// The app goroutines kill and reap their cron jobs on cancel, so wait for them before
		// the process exits and orphans a Setsid child.
		for _, runtime := range runtimes {
			<-runtime.closed
		}
	})
}

// assignPorts fixes one port per process for the daemon lifetime; apps are handled in config order,
// processes in name order, so the first configured app always lands on the first port of the range.
func (m *Manager) assignPorts(spec *apps.App) error {
	for _, name := range slices.Sorted(maps.Keys(spec.Commands)) {
		if _, err := m.ports.Allocate(spec.Name, name); err != nil {
			return fmt.Errorf("%s/%s: %w", spec.Name, name, err)
		}
	}
	return nil
}

// selectCgroup returns the cgroup backend when the host can use it and the defaults do not force
// procgroup. A box without the cgroup v2 hierarchy gets nil and the procgroup fallback.
func selectCgroup(cfg config.Config) res.Backend {
	if cfg.Defaults.Resources == "procgroup" {
		return nil
	}
	if !res.Available(res.DefaultCgroupRoot) {
		return nil
	}
	return res.NewCgroup(res.DefaultCgroupRoot)
}

func (m *Manager) add(ctx context.Context, spec *apps.App) {
	runtimeCtx, cancel := context.WithCancel(ctx)
	runtime := &appRuntime{ctx: runtimeCtx, cancel: cancel, cfg: m.cfg, spec: spec, allocator: m.ports, backend: m.backend, cgroup: m.cgroup, echo: m.echo, secrets: m.secrets, databases: m.databases, sink: m.sink, restart: m.Restart, requests: make(chan request), events: make(chan processEvent, 32), state: Stopped, processes: map[string]*process{}, failures: map[string]int{}, cron: map[string]*jobState{}, hooks: map[string]*jobState{}, closed: make(chan struct{})}
	runtime.lastActivity = m.activities[spec.Name]
	runtime.maintenance = m.maintenance[spec.Name]
	runtime.syncCron(time.Now())
	runtime.syncHooks()
	m.apps[spec.Name] = runtime
	go runtime.loop()
}

func (m *Manager) runtime(name string) (*appRuntime, error) {
	m.mu.RLock()
	runtime := m.apps[name]
	m.mu.RUnlock()
	if runtime == nil {
		return nil, fmt.Errorf("unknown app %q", name)
	}
	return runtime, nil
}

func (m *Manager) Start(name string) error {
	runtime, err := m.runtime(name)
	if err != nil {
		if _, scanErr := m.Rescan(); scanErr != nil {
			return scanErr
		}
		runtime, err = m.runtime(name)
		if err != nil {
			return err
		}
	}
	if err := runtime.call(request{kind: requestStart}); err != nil {
		return err
	}
	return m.setDesired(name, true)
}

func (m *Manager) Stop(name string) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	m.drain(runtime, name)
	if err := runtime.call(request{kind: requestStop}); err != nil {
		return err
	}
	return m.setDesired(name, false)
}

func (m *Manager) Restart(name string) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	m.drain(runtime, name)
	if err := runtime.call(request{kind: requestRestart}); err != nil {
		return err
	}
	return m.setDesired(name, true)
}

// Destroy permanently removes an opted-in app from the host. It shuts down every process and
// job before removing the apps-directory entry, then drops generated state tied to the app.
func (m *Manager) Destroy(name string) error {
	m.rescanMu.Lock()
	defer m.rescanMu.Unlock()
	if m.cfg.Dev() {
		return errors.New("cannot destroy an app in single-app mode")
	}
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	snapshot := runtime.query(request{kind: requestSnapshot}).snapshot
	if !snapshot.Deletable {
		return fmt.Errorf("app %q is not deletable; set deletable: true in its config", name)
	}
	m.drain(runtime, name)
	if err := runtime.call(request{kind: requestStop}); err != nil {
		return err
	}
	if err := m.clearAppState(name); err != nil {
		return err
	}

	var remaining []*apps.App
	m.mu.Lock()
	delete(m.apps, name)
	delete(m.activities, name)
	for _, live := range m.apps {
		remaining = append(remaining, live.spec)
	}
	m.mu.Unlock()
	m.inflightMu.Lock()
	delete(m.inflight, name)
	m.inflightMu.Unlock()
	runtime.cancel()
	<-runtime.closed
	_ = os.Remove(filepath.Join(m.cfg.StateDir, name))
	if err := apps.Destroy(m.cfg.Apps, name); err != nil {
		return fmt.Errorf("destroy %s: %w", name, err)
	}
	m.syncHookSecrets(remaining)
	return nil
}

// drain marks the app as draining so the proxy stops sending new requests, then waits for the
// in-flight ones to finish, bounded by the host stop_timeout. It runs on the caller's goroutine,
// never the app's, so snapshots stay responsive while it waits.
func (m *Manager) drain(runtime *appRuntime, name string) {
	if err := runtime.call(request{kind: requestDrain, on: true}); err != nil {
		return
	}
	timeout := m.cfg.Defaults.StopTimeout.Value()
	if timeout <= 0 {
		return
	}
	counter := m.traffic(name)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if counter.Load() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Enter registers one in-flight proxied request and returns the counter Leave expects.
func (m *Manager) Enter(name string) *atomic.Int64 {
	counter := m.traffic(name)
	counter.Add(1)
	return counter
}

// Leave clears one in-flight request.
func (m *Manager) Leave(counter *atomic.Int64) {
	if counter != nil {
		counter.Add(-1)
	}
}

func (m *Manager) traffic(name string) *atomic.Int64 {
	m.inflightMu.Lock()
	defer m.inflightMu.Unlock()
	counter := m.inflight[name]
	if counter == nil {
		counter = &atomic.Int64{}
		m.inflight[name] = counter
	}
	return counter
}

// Wake starts an app on behalf of the proxy and reports a failed start, which an explicit run or
// console start does not.
func (m *Manager) Wake(name string) {
	if err := m.Start(name); err != nil {
		logx.Warnf("wake %s: %v", name, err)
		m.emit(notify.Event{Type: "wake-failed", App: name, Error: err.Error(), Time: time.Now()})
	}
}

func (m *Manager) emit(event notify.Event) {
	if m.sink != nil {
		m.sink.Send(event)
	}
}

// SetMaintenance flips the proxy into or out of maintenance answers for name. The app itself
// keeps running; the flag survives a host restart through state_dir/maintenance.json.
func (m *Manager) SetMaintenance(name string, on bool) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	if err := runtime.call(request{kind: requestMaintenance, on: on}); err != nil {
		return err
	}
	m.desiredMu.Lock()
	defer m.desiredMu.Unlock()
	if on {
		m.maintenance[name] = true
	} else {
		delete(m.maintenance, name)
	}
	return saveNames(filepath.Join(m.cfg.StateDir, "maintenance.json"), m.maintenance)
}

// RunCron starts one scheduled job immediately. The job's next scheduled run is unchanged.
func (m *Manager) RunCron(name, job string) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	return runtime.call(request{kind: requestCronRun, job: job})
}

// RunHook starts one deploy hook now, whether the app is running or stopped.
func (m *Manager) RunHook(name, hookName string) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	return runtime.call(request{kind: requestHookRun, hook: hookName})
}

// Hooks returns every hook of one app with its effective secret. It is the only read path that
// carries secrets, so it stays off the regular snapshot.
func (m *Manager) Hooks(name string) ([]HookInfo, error) {
	runtime, err := m.runtime(name)
	if err != nil {
		return nil, err
	}
	response := runtime.query(request{kind: requestHooks})
	return response.hooks, response.err
}

// HookSecret returns the effective secret of one hook, for verifying a ping.
func (m *Manager) HookSecret(name, hookName string) (string, error) {
	hooks, err := m.Hooks(name)
	if err != nil {
		return "", err
	}
	for _, info := range hooks {
		if info.Name == hookName {
			if info.Secret == "" {
				return "", fmt.Errorf("hook %q has no secret", hookName)
			}
			return info.Secret, nil
		}
	}
	return "", fmt.Errorf("unknown hook %q", hookName)
}

// RotateHook mints a new generated secret for one hook and returns the hook with its new URL. A
// hook whose secret comes from the config is rejected: the operator changes it in the file.
func (m *Manager) RotateHook(name, hookName string) (HookInfo, error) {
	runtime, err := m.runtime(name)
	if err != nil {
		return HookInfo{}, err
	}
	response := runtime.query(request{kind: requestHookRotate, hook: hookName})
	if response.err != nil {
		return HookInfo{}, response.err
	}
	hooks, err := m.Hooks(name)
	if err != nil {
		return HookInfo{}, err
	}
	for _, info := range hooks {
		if info.Name == hookName {
			return info, nil
		}
	}
	return HookInfo{}, fmt.Errorf("unknown hook %q", hookName)
}

// Exec runs one command in the app's environment and returns its combined output. It resolves
// the executable against the app PATH, captures both streams and kills the process group on
// timeout. It runs off the app's goroutine so a slow command cannot stall the supervisor.
// appDatabases is appRuntime.appDatabases for the paths that hold a spec but no runtime, namely
// Exec. It runs off the app goroutine, like the rest of Exec.
func (m *Manager) appDatabases(spec *apps.App) (map[string]string, error) {
	wanted := spec.Config.PgDatabases()
	if len(wanted) == 0 {
		return nil, nil
	}
	if m.databases == nil {
		return nil, errors.New("pg_db needs the PostgreSQL service, which is not available")
	}
	return m.databases.AppDatabases(m.ctx, spec.Name, wanted)
}

func (m *Manager) Exec(name string, argv []string, timeout time.Duration) (ExecResult, error) {
	if len(argv) == 0 {
		return ExecResult{}, errors.New("no command given")
	}
	runtime, err := m.runtime(name)
	if err != nil {
		return ExecResult{}, err
	}
	response := runtime.query(request{kind: requestExecInfo})
	if response.err != nil {
		return ExecResult{}, response.err
	}
	spec := response.app
	generated, err := m.appDatabases(spec)
	if err != nil {
		return ExecResult{}, err
	}
	env := processEnv(spec, "exec", 0, m.cfg.Socket, spec.Config.Env, generated)
	resolved, err := resolveExecutable(argv[0], spec.Dir, env["PATH"])
	if err != nil {
		return ExecResult{}, err
	}
	command := exec.Command(resolved, argv[1:]...)
	command.Dir = spec.Dir
	command.Env = envSlice(env)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		return ExecResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(timeout):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		return ExecResult{Output: output.String(), ExitCode: -1}, fmt.Errorf("command timed out after %s", timeout)
	}
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return ExecResult{Output: output.String(), ExitCode: exitCode}, nil
}

// syncHookSecrets mints and prunes generated hook secrets against the discovered specs, using
// the same list the runtimes were built from so no running spec is read off its goroutine.
func (m *Manager) syncHookSecrets(discovered []*apps.App) {
	if m.secrets == nil {
		return
	}
	live := map[string]map[string]bool{}
	for _, spec := range discovered {
		hooks := map[string]bool{}
		for name, hookConfig := range spec.Hooks {
			if hookConfig.Disabled {
				continue
			}
			hooks[name] = true
			if configured, ok := spec.Config.Hooks[name]; ok && configured.Secret != "" {
				continue
			}
			_, _ = m.secrets.Ensure(spec.Name, name)
		}
		live[spec.Name] = hooks
	}
	_ = m.secrets.Reconcile(live)
}

// RestartRequired lists the host keys whose value on disk differs from the running session.
func (m *Manager) RestartRequired() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.restartRequired...)
}

func (m *Manager) Snapshots() []Snapshot {
	m.mu.RLock()
	runtimes := make([]*appRuntime, 0, len(m.apps))
	for _, runtime := range m.apps {
		runtimes = append(runtimes, runtime)
	}
	m.mu.RUnlock()
	result := make([]Snapshot, 0, len(runtimes))
	for _, runtime := range runtimes {
		response := runtime.query(request{kind: requestSnapshot})
		result = append(result, response.snapshot)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (m *Manager) Snapshot(name string) (Snapshot, error) {
	runtime, err := m.runtime(name)
	if err != nil {
		return Snapshot{}, err
	}
	response := runtime.query(request{kind: requestSnapshot})
	return response.snapshot, response.err
}

func (m *Manager) Logs(name, processName string, lines int) (map[string][]string, error) {
	runtime, err := m.runtime(name)
	if err != nil {
		return nil, err
	}
	response := runtime.query(request{kind: requestLogs, processName: processName, lines: lines})
	return response.logs, response.err
}

// SealLogs seals every process log segment of one app and returns the sealed file paths. The
// ingestion module calls it on a schedule, then reads and deletes the segments.
func (m *Manager) SealLogs(name string) ([]string, error) {
	runtime, err := m.runtime(name)
	if err != nil {
		return nil, err
	}
	response := runtime.query(request{kind: requestSealLogs})
	return response.sealed, response.err
}

func (m *Manager) Touch(name string) {
	if runtime, err := m.runtime(name); err == nil {
		select {
		case runtime.requests <- request{kind: requestTouch}:
		default:
		}
	}
}

func (m *Manager) ResolveHost(host string) (Snapshot, bool) {
	host = config.NormalizeHost(host)
	bestScore := -1
	var best Snapshot
	for _, snapshot := range m.Snapshots() {
		for _, pattern := range snapshot.Hosts {
			score, match := config.MatchHost(host, strings.ToLower(pattern))
			if match && score > bestScore {
				best, bestScore = snapshot, score
			}
		}
	}
	return best, bestScore >= 0
}

func (m *Manager) Ports() map[string]int { return m.ports.Entries() }

// HostConfig returns the host keys as last reloaded, so a module can re-apply settings that are
// hot but not part of an app snapshot.
func (m *Manager) HostConfig() config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.hostConfig
}

// Rescan re-reads the root file and every app folder. App-level keys, including host defaults,
// apply live; host keys are left as started and reported through RestartRequired.
func (m *Manager) Rescan() ([]error, error) {
	m.rescanMu.Lock()
	defer m.rescanMu.Unlock()
	scanConfig := m.cfg
	var restartRequired []string
	var loaded config.Config
	reloaded := false
	if m.cfg.SourcePath != "" {
		current, err := config.Load(m.cfg.SourcePath)
		if err != nil {
			return nil, err
		}
		loaded, reloaded = current, true
		scanConfig.App, scanConfig.Defaults = loaded.App, loaded.Defaults
		restartRequired = config.RestartRequired(m.cfg, loaded)
	}
	discovered, invalid, err := apps.Discover(scanConfig)
	if err != nil {
		return invalid, err
	}
	// Mutate the app table under the lock, but call into each runtime outside it: a runtime call
	// can block on the app goroutine (a stop waits for processes), and holding m.mu would stall
	// every snapshot and proxy host lookup behind it.
	type update struct {
		runtime *appRuntime
		spec    *apps.App
	}
	var updates []update
	var removed []*appRuntime
	m.mu.Lock()
	if reloaded {
		m.hostConfig = loaded
	}
	m.cfg.App, m.cfg.Defaults = scanConfig.App, scanConfig.Defaults
	if len(restartRequired) > 0 && strings.Join(restartRequired, ",") != strings.Join(m.restartRequired, ",") {
		logx.Infof("rescan: %s changed in %s, restart dboss to apply", strings.Join(restartRequired, ", "), m.cfg.SourcePath)
	}
	m.restartRequired = restartRequired
	seen := map[string]bool{}
	for _, spec := range discovered {
		seen[spec.Name] = true
		if runtime := m.apps[spec.Name]; runtime != nil {
			updates = append(updates, update{runtime: runtime, spec: spec})
		} else if err := m.assignPorts(spec); err != nil {
			invalid = append(invalid, apps.ScanError{Name: spec.Name, Err: err})
		} else {
			m.add(m.ctx, spec)
		}
	}
	for name, runtime := range m.apps {
		if !seen[name] {
			removed = append(removed, runtime)
			delete(m.apps, name)
		}
	}
	m.mu.Unlock()
	for _, item := range updates {
		item.runtime.call(request{kind: requestUpdate, spec: item.spec})
	}
	for _, runtime := range removed {
		_ = runtime.call(request{kind: requestStop})
		runtime.cancel()
		<-runtime.closed
	}
	m.syncHookSecrets(discovered)
	return invalid, nil
}

func (m *Manager) cronLoop(ctx context.Context) {
	ticker := time.NewTicker(cronTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.mu.RLock()
			runtimes := make([]*appRuntime, 0, len(m.apps))
			for _, runtime := range m.apps {
				runtimes = append(runtimes, runtime)
			}
			m.mu.RUnlock()
			for _, runtime := range runtimes {
				_ = runtime.call(request{kind: requestCron, now: now})
			}
		}
	}
}

func (m *Manager) idleLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.Daemon.IdleTick.Value())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.mu.RLock()
			runtimes := make([]*appRuntime, 0, len(m.apps))
			for _, runtime := range m.apps {
				runtimes = append(runtimes, runtime)
			}
			m.mu.RUnlock()
			for _, runtime := range runtimes {
				// An in-flight request, a websocket being the long-lived case, counts as
				// activity for as long as it runs, so idle_stop cannot cut it off.
				if m.traffic(runtime.spec.Name).Load() > 0 {
					continue
				}
				result := runtime.query(request{kind: requestIdle, now: now})
				if result.idleStopped {
					_ = m.setDesired(result.snapshot.Name, false)
				}
			}
			_ = m.saveActivities()
		}
	}
}

type requestKind int

const (
	requestStart requestKind = iota
	requestStop
	requestRestart
	requestSnapshot
	requestLogs
	requestTouch
	requestIdle
	requestUpdate
	requestMaintenance
	requestSealLogs
	requestCron
	requestCronRun
	requestHookRun
	requestHooks
	requestHookRotate
	requestExecInfo
	requestDrain
)

type request struct {
	kind        requestKind
	reply       chan response
	processName string
	job         string
	hook        string
	lines       int
	now         time.Time
	spec        *apps.App
	on          bool
}
type response struct {
	snapshot    Snapshot
	logs        map[string][]string
	sealed      []string
	hooks       []HookInfo
	app         *apps.App
	err         error
	idleStopped bool
}

// processEvent is bound to the process that produced it; the runtime drops events from a
// process it no longer tracks so a late exit or healthcheck cannot act on its replacement.
type processEvent struct {
	kind     string
	proc     *process
	job      *jobRun
	err      error
	exitCode int
}
type process struct {
	name      string
	command   apps.Command
	cmd       *exec.Cmd
	pid       int
	port      int
	startedAt time.Time
	restarts  int
	log       io.WriteCloser
	done      chan struct{} // closed when the runtime stops tracking the process
	waited    chan struct{} // closed once cmd.Wait has reaped the process
}

type appRuntime struct {
	ctx              context.Context
	cancel           context.CancelFunc
	cfg              config.Config
	spec             *apps.App
	allocator        *ports.Allocator
	backend          res.Backend
	cgroup           res.Backend
	echo             *Echo
	requests         chan request
	events           chan processEvent
	state            State
	processes        map[string]*process
	cron             map[string]*jobState
	hooks            map[string]*jobState
	secrets          *hook.Store
	databases        Databases
	sink             notify.Sink
	restart          func(string) error
	failures         map[string]int
	ready            map[string]bool
	lastActivity     time.Time
	lastError        string
	lastErrorProcess string
	maintenance      bool
	draining         bool
	closed           chan struct{}
}

func (a *appRuntime) call(req request) error { return a.query(req).err }
func (a *appRuntime) query(req request) response {
	req.reply = make(chan response, 1)
	select {
	case a.requests <- req:
		return <-req.reply
	case <-a.ctx.Done():
		return response{err: errors.New("supervisor closed")}
	}
}

// sendEvent delivers one event to the runtime loop, giving up if the runtime is shutting down so
// a background probe can never block forever on a full channel.
func (a *appRuntime) sendEvent(event processEvent) {
	select {
	case a.events <- event:
	case <-a.ctx.Done():
	}
}

func (a *appRuntime) loop() {
	defer close(a.closed)
	for {
		select {
		case <-a.ctx.Done():
			a.stopJobs()
			for _, process := range a.processes {
				if process.log != nil {
					_ = process.log.Close()
					process.log = nil
				}
			}
			return
		case req := <-a.requests:
			result := a.handle(req)
			if req.reply != nil {
				req.reply <- result
			}
		case event := <-a.events:
			a.handleEvent(event)
		}
	}
}

func (a *appRuntime) handle(req request) response {
	switch req.kind {
	case requestStart:
		return response{err: a.start()}
	case requestDrain:
		a.draining = req.on
	case requestStop:
		a.draining = false
		return response{err: a.stop()}
	case requestRestart:
		a.draining = false
		if err := a.stop(); err != nil {
			return response{err: err}
		}
		return response{err: a.start()}
	case requestSnapshot:
		return response{snapshot: a.snapshot()}
	case requestLogs:
		logs, err := a.logs(req.processName, req.lines)
		return response{logs: logs, err: err}
	case requestTouch:
		a.lastActivity = time.Now()
	case requestIdle:
		if a.state == Running && a.spec.Config.IdleStop > 0 && !a.lastActivity.IsZero() && req.now.Sub(a.lastActivity) >= a.spec.Config.IdleStop.Value() {
			err := a.stop()
			return response{err: err, idleStopped: true, snapshot: a.snapshot()}
		}
	case requestUpdate:
		a.spec = req.spec
		a.syncCron(time.Now())
		a.syncHooks()
	case requestMaintenance:
		a.maintenance = req.on
	case requestSealLogs:
		sealed, err := a.sealLogs()
		return response{sealed: sealed, err: err}
	case requestCron:
		a.cronTick(req.now)
	case requestCronRun:
		return response{err: a.runCron(req.job, time.Now())}
	case requestHookRun:
		return response{err: a.runHook(req.hook, time.Now())}
	case requestHooks:
		return response{hooks: a.hookInfos()}
	case requestHookRotate:
		return response{err: a.rotateHook(req.hook)}
	case requestExecInfo:
		return response{app: a.spec}
	}
	return response{}
}

// rotateHook mints a new generated secret for one hook. A hook whose secret lives in the config
// cannot be rotated here.
func (a *appRuntime) rotateHook(name string) error {
	if a.hooks[name] == nil {
		return fmt.Errorf("unknown hook %q", name)
	}
	if configured, ok := a.spec.Config.Hooks[name]; ok && configured.Secret != "" {
		return fmt.Errorf("hook %q uses a secret from the config; change it there", name)
	}
	if a.secrets == nil {
		return errors.New("hook secret store is not available")
	}
	secret, err := hook.Generate()
	if err != nil {
		return err
	}
	return a.secrets.Set(a.spec.Name, name, secret)
}

// sealLogs renames every process's current log segment aside and opens a fresh one. It returns
// every sealed segment on disk, oldest first per log, so one left behind by a failed ingest
// commit is handed out again.
func (a *appRuntime) sealLogs() ([]string, error) {
	for _, name := range slices.Sorted(maps.Keys(a.processes)) {
		writer, ok := a.processes[name].log.(*logWriter)
		if !ok {
			continue
		}
		if _, err := writer.Seal(); err != nil {
			return nil, err
		}
	}
	if err := a.sealJobLogs(); err != nil {
		return nil, err
	}
	return filepath.Glob(filepath.Join(a.cfg.LogDir, a.spec.Name, "*.sealed"))
}

func (a *appRuntime) start() error {
	if a.state == Running || a.state == Starting {
		return nil
	}
	a.failures = map[string]int{}
	a.ready = map[string]bool{}
	a.state, a.lastError, a.lastErrorProcess = Starting, "", ""
	for _, name := range a.startOrder() {
		command := a.spec.Commands[name]
		port, err := a.allocator.Allocate(a.spec.Name, name)
		if err != nil {
			a.lastErrorProcess = name
			return a.failStart(err)
		}
		if err := a.spawn(name, command, port); err != nil {
			a.lastErrorProcess = name
			return a.failStart(err)
		}
	}
	if len(a.spec.Config.WebProcesses) == 0 {
		a.state = Running
		a.lastActivity = time.Now()
		return nil
	}
	for _, web := range a.spec.Config.WebProcesses {
		if process := a.processes[web.Name]; process != nil {
			go a.monitor(process, a.spec.Config.Process(web.Name), a.webHost(web.Name))
		}
	}
	return nil
}

// startOrder lists the processes in the order start spawns them: every web process first, then the
// rest by name, so a web process that expects earlier setup still gets it. Port assignment is
// unaffected because assignPorts keeps its own name order.
func (a *appRuntime) startOrder() []string {
	names := slices.Sorted(maps.Keys(a.spec.Commands))
	isWeb := make(map[string]bool, len(a.spec.Config.WebProcesses))
	for _, web := range a.spec.Config.WebProcesses {
		isWeb[web.Name] = true
	}
	ordered := make([]string, 0, len(names))
	for _, name := range names {
		if isWeb[name] {
			ordered = append(ordered, name)
		}
	}
	for _, name := range names {
		if !isWeb[name] {
			ordered = append(ordered, name)
		}
	}
	return ordered
}

// allWebReady reports whether every web process has passed its readiness check, so the app flips
// to running only once all of them can serve.
func (a *appRuntime) allWebReady() bool {
	for _, web := range a.spec.Config.WebProcesses {
		if !a.ready[web.Name] {
			return false
		}
	}
	return true
}

// webHost is the Host header the readiness probe and proxy use for one web process.
func (a *appRuntime) webHost(name string) string {
	for _, web := range a.spec.Config.WebProcesses {
		if web.Name == name && len(web.Hosts) > 0 {
			return config.BaseHost(web.Hosts[0])
		}
	}
	return ""
}

func (a *appRuntime) failStart(err error) error {
	a.lastError = err.Error()
	_ = a.stopProcesses()
	a.state = Crashed
	a.emit("crash", a.lastError)
	return err
}

// emit forwards one runtime event to the notifier; a nil sink drops it.
func (a *appRuntime) emit(eventType, message string) {
	if a.sink == nil {
		return
	}
	a.sink.Send(notify.Event{Type: eventType, App: a.spec.Name, Error: message, Time: time.Now()})
}

func (a *appRuntime) stop() error {
	if a.state == Stopped {
		return nil
	}
	a.state = Stopping
	err := a.stopProcesses()
	a.state = Stopped
	return err
}

func (a *appRuntime) stopProcesses() error {
	names := make([]string, 0, len(a.processes))
	for name := range a.processes {
		names = append(names, name)
	}
	var first error
	for _, name := range names {
		p := a.processes[name]
		defaults := a.spec.Config.Process(name)
		signal, _ := res.Signal(defaults.StopSignal)
		if err := syscall.Kill(-p.pid, signal); err != nil && err != syscall.ESRCH && first == nil {
			first = err
		}
	}
	deadline := time.Now().Add(a.maxStopTimeout(names))
	for _, name := range names {
		p := a.processes[name]
		if !p.waitUntil(deadline) {
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
			<-p.waited
		}
		a.cleanupProcess(name)
	}
	return first
}
