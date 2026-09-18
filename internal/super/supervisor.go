package super

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"app-boss/internal/apps"
	"app-boss/internal/config"
	"app-boss/internal/ports"
	"app-boss/internal/res"
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
	Name      string    `json:"name"`
	Command   string    `json:"command"`
	PID       int       `json:"pid,omitempty"`
	Port      int       `json:"port"`
	Restarts  int       `json:"restarts"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

type RequestRates struct {
	LastMinute int64 `json:"last_minute"`
	LastHour   int64 `json:"last_hour"`
	LastDay    int64 `json:"last_day"`
}

type Snapshot struct {
	Name            string            `json:"name"`
	State           State             `json:"state"`
	Maintenance     bool              `json:"maintenance"`
	Dir             string            `json:"dir"`
	Hosts           []string          `json:"hosts"`
	CanonicalHost   string            `json:"canonical_host,omitempty"`
	WebProcess      string            `json:"web_process"`
	Web             config.Web        `json:"web"`
	Processes       []ProcessSnapshot `json:"processes"`
	LastActivity    time.Time         `json:"last_activity,omitempty"`
	Uptime          string            `json:"uptime,omitempty"`
	Resources       res.Stats         `json:"resources"`
	RequestRates    RequestRates      `json:"request_rates"`
	Error           string            `json:"error,omitempty"`
	ErrorLog        []string          `json:"error_log,omitempty"`
	LogRetention    time.Duration     `json:"-"`
	StdoutRetention time.Duration     `json:"-"`
	LogFlush        time.Duration     `json:"-"`
}

const failureLogLines = 1000

type Manager struct {
	cfg             config.Config
	ports           *ports.Allocator
	backend         res.Backend
	echo            *Echo
	closeOnce       sync.Once
	mu              sync.RWMutex
	apps            map[string]*appRuntime
	restartRequired []string
	desiredMu       sync.Mutex
	desired         map[string]bool
	maintenance     map[string]bool
	activities      map[string]time.Time
	ctx             context.Context
	cancel          context.CancelFunc
}

// New discovers the apps and starts the ones that were running before. A non-nil echo mirrors
// every process's output to it, which the foreground session uses when attached to a terminal.
func New(cfg config.Config, allocator *ports.Allocator, echo *Echo) (*Manager, []error, error) {
	discovered, invalid, err := apps.Discover(cfg)
	if err != nil {
		return nil, invalid, err
	}
	runningPath := filepath.Join(cfg.StateDir, "running.json")
	desired, err := loadNames(runningPath)
	if err != nil {
		return nil, invalid, err
	}
	// A host with no running list yet starts everything it found. Once the list exists it is
	// authoritative, so an app someone stopped stays stopped across restarts.
	if _, statErr := os.Stat(runningPath); errors.Is(statErr, os.ErrNotExist) {
		for _, spec := range discovered {
			desired[spec.Name] = true
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
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{cfg: cfg, ports: allocator, backend: res.Procgroup{}, echo: echo, apps: map[string]*appRuntime{}, desired: desired, maintenance: maintenance, activities: activities, ctx: ctx, cancel: cancel}
	for _, spec := range discovered {
		if err := m.assignPorts(spec); err != nil {
			cancel()
			return nil, invalid, err
		}
		m.add(ctx, spec)
	}
	if cfg.Daemon.ResumeRunning {
		for name := range desired {
			if runtime := m.apps[name]; runtime != nil {
				_ = runtime.call(request{kind: requestStart})
			}
		}
	}
	go m.idleLoop(ctx)
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

func (m *Manager) add(ctx context.Context, spec *apps.App) {
	runtimeCtx, cancel := context.WithCancel(ctx)
	runtime := &appRuntime{ctx: runtimeCtx, cancel: cancel, cfg: m.cfg, spec: spec, allocator: m.ports, backend: m.backend, echo: m.echo, requests: make(chan request), events: make(chan processEvent, 32), state: Stopped, processes: map[string]*process{}, failures: map[string]int{}}
	runtime.lastActivity = m.activities[spec.Name]
	runtime.maintenance = m.maintenance[spec.Name]
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
	if err := runtime.call(request{kind: requestRestart}); err != nil {
		return err
	}
	return m.setDesired(name, true)
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
	host = strings.ToLower(strings.TrimSuffix(strings.Split(host, ":")[0], "."))
	bestScore := -1
	var best Snapshot
	for _, snapshot := range m.Snapshots() {
		for _, pattern := range snapshot.Hosts {
			score, match := hostMatch(host, strings.ToLower(pattern))
			if match && score > bestScore {
				best, bestScore = snapshot, score
			}
		}
	}
	return best, bestScore >= 0
}

func (m *Manager) Ports() map[string]int { return m.ports.Entries() }

// Rescan re-reads the root file and every app folder. App-level keys, including host defaults,
// apply live; host keys are left as started and reported through RestartRequired.
func (m *Manager) Rescan() ([]error, error) {
	scanConfig := m.cfg
	var restartRequired []string
	if m.cfg.SourcePath != "" {
		loaded, err := config.Load(m.cfg.SourcePath)
		if err != nil {
			return nil, err
		}
		scanConfig.App, scanConfig.Defaults = loaded.App, loaded.Defaults
		restartRequired = config.RestartRequired(m.cfg, loaded)
	}
	discovered, invalid, err := apps.Discover(scanConfig)
	if err != nil {
		return invalid, err
	}
	m.mu.Lock()
	m.cfg.App, m.cfg.Defaults = scanConfig.App, scanConfig.Defaults
	if len(restartRequired) > 0 && strings.Join(restartRequired, ",") != strings.Join(m.restartRequired, ",") {
		log.Printf("rescan: %s changed in %s, restart appboss to apply", strings.Join(restartRequired, ", "), m.cfg.SourcePath)
	}
	m.restartRequired = restartRequired
	seen := map[string]bool{}
	for _, spec := range discovered {
		seen[spec.Name] = true
		if runtime := m.apps[spec.Name]; runtime != nil {
			runtime.call(request{kind: requestUpdate, spec: spec})
		} else if err := m.assignPorts(spec); err != nil {
			invalid = append(invalid, apps.ScanError{Name: spec.Name, Err: err})
		} else {
			m.add(m.ctx, spec)
		}
	}
	for name, runtime := range m.apps {
		if !seen[name] {
			_ = runtime.call(request{kind: requestStop})
			runtime.cancel()
			delete(m.apps, name)
		}
	}
	m.mu.Unlock()
	return invalid, nil
}

func (m *Manager) setDesired(name string, running bool) error {
	m.desiredMu.Lock()
	defer m.desiredMu.Unlock()
	if running {
		m.desired[name] = true
	} else {
		delete(m.desired, name)
	}
	return saveNames(filepath.Join(m.cfg.StateDir, "running.json"), m.desired)
}

// saveNames writes the sorted keys of set to path as a JSON list.
func saveNames(path string, set map[string]bool) error {
	return writeStateFile(path, slices.Sorted(maps.Keys(set)))
}

// writeStateFile marshals value as indented JSON and replaces path atomically, so a crash never
// leaves a half-written state file behind.
func writeStateFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	newPath := path + ".new"
	if err := os.WriteFile(newPath, data, 0o640); err != nil {
		return err
	}
	return os.Rename(newPath, path)
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
				result := runtime.query(request{kind: requestIdle, now: now})
				if result.idleStopped {
					_ = m.setDesired(result.snapshot.Name, false)
				}
			}
			_ = m.saveActivities()
		}
	}
}

func loadNames(path string) (map[string]bool, error) {
	result := map[string]bool{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, err
	}
	for _, name := range names {
		result[name] = true
	}
	return result, nil
}

func loadActivities(stateDir string) (map[string]time.Time, error) {
	result := map[string]time.Time{}
	data, err := os.ReadFile(filepath.Join(stateDir, "last_activity.json"))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (m *Manager) saveActivities() error {
	m.mu.RLock()
	runtimes := make([]*appRuntime, 0, len(m.apps))
	for _, runtime := range m.apps {
		runtimes = append(runtimes, runtime)
	}
	m.mu.RUnlock()
	activities := map[string]time.Time{}
	for _, runtime := range runtimes {
		snapshot := runtime.query(request{kind: requestSnapshot}).snapshot
		if !snapshot.LastActivity.IsZero() {
			activities[snapshot.Name] = snapshot.LastActivity
		}
	}
	return writeStateFile(filepath.Join(m.cfg.StateDir, "last_activity.json"), activities)
}

func hostMatch(host, pattern string) (int, bool) {
	if host == pattern {
		return len(pattern) + 10000, true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		return len(suffix), strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	return 0, false
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
)

type request struct {
	kind        requestKind
	reply       chan response
	processName string
	lines       int
	now         time.Time
	spec        *apps.App
	on          bool
}
type response struct {
	snapshot    Snapshot
	logs        map[string][]string
	sealed      []string
	err         error
	idleStopped bool
}

// processEvent is bound to the process that produced it; the runtime drops events from a
// process it no longer tracks so a late exit or healthcheck cannot act on its replacement.
type processEvent struct {
	kind     string
	proc     *process
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
	echo             *Echo
	requests         chan request
	events           chan processEvent
	state            State
	processes        map[string]*process
	failures         map[string]int
	lastActivity     time.Time
	lastError        string
	lastErrorProcess string
	maintenance      bool
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

func (a *appRuntime) loop() {
	for {
		select {
		case <-a.ctx.Done():
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
	case requestStop:
		return response{err: a.stop()}
	case requestRestart:
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
	case requestMaintenance:
		a.maintenance = req.on
	case requestSealLogs:
		sealed, err := a.sealLogs()
		return response{sealed: sealed, err: err}
	}
	return response{}
}

// sealLogs renames every process's current log segment aside and opens a fresh one, returning
// the sealed paths for the ingestion module.
func (a *appRuntime) sealLogs() ([]string, error) {
	var sealed []string
	for _, name := range slices.Sorted(maps.Keys(a.processes)) {
		writer, ok := a.processes[name].log.(*logWriter)
		if !ok {
			continue
		}
		path, err := writer.Seal()
		if err != nil {
			return sealed, err
		}
		if path != "" {
			sealed = append(sealed, path)
		}
	}
	return sealed, nil
}

func (a *appRuntime) start() error {
	if a.state == Running || a.state == Starting {
		return nil
	}
	a.failures = map[string]int{}
	a.state, a.lastError, a.lastErrorProcess = Starting, "", ""
	for name, command := range a.spec.Commands {
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
	if web := a.processes[a.spec.Config.WebProcess]; web == nil {
		a.state = Running
		a.lastActivity = time.Now()
	} else {
		go a.readiness(web)
	}
	return nil
}

func (a *appRuntime) failStart(err error) error {
	a.lastError = err.Error()
	_ = a.stopProcesses()
	a.state = Crashed
	return err
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

func (p *process) waitUntil(deadline time.Time) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-p.waited:
		return true
	case <-timer.C:
		return false
	}
}

func (a *appRuntime) maxStopTimeout(names []string) time.Duration {
	maximum := time.Duration(0)
	for _, name := range names {
		if d := a.spec.Config.Process(name).StopTimeout.Value(); d > maximum {
			maximum = d
		}
	}
	return maximum
}

func (a *appRuntime) spawn(name string, command apps.Command, port int) error {
	defaults := a.spec.Config.Process(name)
	var cmd *exec.Cmd
	if defaults.Shell {
		cmd = exec.Command("/bin/sh", "-c", command.Line)
	} else {
		resolved, err := resolveExecutable(command.Argv[0], a.spec.Dir, a.spec.Env["PATH"])
		if err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}
		command.Argv = append([]string(nil), command.Argv...)
		command.Argv[0] = resolved
		cmd = exec.Command(resolved, command.Argv[1:]...)
	}
	cmd.Dir = a.spec.Dir
	cmd.Env = environment(a.spec, name, port, a.cfg.Socket, defaults.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The port is fixed for this process, so whatever holds it is stale and gets killed first.
	killed, err := clearPort(port, defaults.StopTimeout.Value())
	if err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	if len(killed) > 0 {
		log.Printf("%s/%s: killed pids %v holding port %d", a.spec.Name, name, killed, port)
	}
	logFile, err := newLogWriter(filepath.Join(a.cfg.LogDir, a.spec.Name, name+".log"), int64(defaults.LogMaxSize), defaults.LogKeep)
	if err != nil {
		return err
	}
	if a.echo != nil {
		logFile.echo = a.echo.writer(a.spec.Name, name)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start %s: %w", name, err)
	}
	if err := a.backend.Place(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
		return err
	}
	p := &process{name: name, command: command, cmd: cmd, pid: cmd.Process.Pid, port: port, startedAt: time.Now(), restarts: a.failures[name], log: logFile, done: make(chan struct{}), waited: make(chan struct{})}
	if err := a.writePID(p); err != nil {
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = logFile.Close()
		return err
	}
	a.processes[name] = p
	go func() {
		err := cmd.Wait()
		close(p.waited)
		exitCode := 0
		if err != nil {
			exitCode = -1
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
		}
		select {
		case a.events <- processEvent{kind: "exit", proc: p, err: err, exitCode: exitCode}:
		case <-a.ctx.Done():
		}
	}()
	return nil
}

func (a *appRuntime) readiness(p *process) {
	defaults := a.spec.Config.Process(p.name)
	deadline := time.Now().Add(defaults.HealthTimeout.Value())
	ticker := time.NewTicker(defaults.HealthInterval.Value())
	defer ticker.Stop()
	host := ""
	if len(a.spec.Config.Hosts) > 0 {
		host = a.spec.Config.Hosts[0]
	}
	lastError := errors.New("healthcheck timed out")
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-p.done:
			return
		case <-ticker.C:
			ok, err := healthCheck(defaults.Health, p.port, host, a.cfg.Proxy.Upstream.DialTimeout.Value())
			if ok {
				a.events <- processEvent{kind: "ready", proc: p}
				return
			}
			if err != nil {
				lastError = err
			}
			if time.Now().After(deadline) {
				a.events <- processEvent{kind: "health-failed", proc: p, err: lastError}
				return
			}
		}
	}
}

// healthCheck talks to the process the way the proxy does: loopback address, app hostname in Host.
func healthCheck(check string, port int, host string, timeout time.Duration) (bool, error) {
	address := fmt.Sprintf("127.0.0.1:%d", port)
	if check == "tcp" {
		connection, err := net.DialTimeout("tcp", address, timeout)
		if err == nil {
			_ = connection.Close()
			return true, nil
		}
		return false, fmt.Errorf("Healthcheck on tcp failed: %w", err)
	}
	path := strings.TrimPrefix(check, "http:")
	client := &http.Client{Timeout: timeout}
	request, err := http.NewRequest(http.MethodGet, "http://"+address+path, nil)
	if err != nil {
		return false, err
	}
	if host != "" {
		request.Host = host
	}
	response, err := client.Do(request)
	if err != nil {
		return false, fmt.Errorf("Healthcheck on %s failed: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("Healthcheck on %s returned %d", path, response.StatusCode)
	}
	return true, nil
}

func (a *appRuntime) handleEvent(event processEvent) {
	name := event.proc.name
	if event.kind != "restart" && a.processes[name] != event.proc {
		return
	}
	switch event.kind {
	case "ready":
		if a.state == Starting {
			a.state = Running
			a.lastActivity = time.Now()
		}
	case "health-failed":
		a.lastError = event.err.Error()
		a.lastErrorProcess = name
		_ = syscall.Kill(-event.proc.pid, syscall.SIGKILL)
	case "restart":
		if a.state != Running && a.state != Starting || a.processes[name] != nil {
			return
		}
		command := a.spec.Commands[name]
		port, err := a.allocator.Allocate(a.spec.Name, name)
		if err == nil {
			err = a.spawn(name, command, port)
			if err == nil && name == a.spec.Config.WebProcess {
				go a.readiness(a.processes[name])
			}
		}
		if err != nil {
			a.lastError = err.Error()
			a.lastErrorProcess = name
			_ = a.stopProcesses()
			a.state = Crashed
		}
	case "exit":
		a.processExited(event)
	}
}

func (a *appRuntime) processExited(event processEvent) {
	name := event.proc.name
	a.cleanupProcess(name)
	if a.state == Stopping || a.state == Stopped {
		return
	}
	defaults := a.spec.Config.Process(name)
	if time.Since(event.proc.startedAt) >= defaults.RestartReset.Value() {
		a.failures[name] = 0
	}
	shouldRestart := defaults.Restart == "always" || (defaults.Restart == "on-failure" && event.exitCode != 0)
	if name == a.spec.Config.WebProcess && shouldRestart {
		a.state = Starting
	}
	if !shouldRestart {
		if name == a.spec.Config.WebProcess {
			a.state = Crashed
			a.lastError = fmt.Sprintf("%s exited with code %d", name, event.exitCode)
			a.lastErrorProcess = name
		} else if len(a.processes) == 0 {
			a.state = Stopped
		}
		return
	}
	a.failures[name]++
	if a.failures[name] >= defaults.MaxRestarts {
		a.state, a.lastError = Crashed, fmt.Sprintf("%s exceeded max_restarts", name)
		a.lastErrorProcess = name
		_ = a.stopProcesses()
		a.state = Crashed
		return
	}
	delay := backoff(defaults.RestartBackoff, a.failures[name])
	time.AfterFunc(delay, func() {
		select {
		case a.events <- processEvent{kind: "restart", proc: event.proc}:
		case <-a.ctx.Done():
		}
	})
}

func backoff(values []any, attempt int) time.Duration {
	first, multiplier, maximum := time.Second, 2.0, 60*time.Second
	if len(values) == 3 {
		if parsed, err := time.ParseDuration(fmt.Sprint(values[0])); err == nil {
			first = parsed
		}
		if parsed, err := strconv.ParseFloat(fmt.Sprint(values[1]), 64); err == nil {
			multiplier = parsed
		}
		if parsed, err := time.ParseDuration(fmt.Sprint(values[2])); err == nil {
			maximum = parsed
		}
	}
	delay := float64(first)
	for i := 1; i < attempt; i++ {
		delay *= multiplier
	}
	if time.Duration(delay) > maximum {
		return maximum
	}
	return time.Duration(delay)
}

func (a *appRuntime) snapshot() Snapshot {
	result := Snapshot{Name: a.spec.Name, State: a.state, Maintenance: a.maintenance, Dir: a.spec.Dir, Hosts: a.spec.Config.Hosts, CanonicalHost: a.spec.Config.CanonicalHost, WebProcess: a.spec.Config.WebProcess, Web: a.spec.Config.Web, LastActivity: a.lastActivity, Error: a.lastError, LogRetention: a.spec.Config.LogRetention.Value(), StdoutRetention: a.spec.Config.StdoutRetention.Value(), LogFlush: a.spec.Config.LogFlush.Value()}
	if result.Error != "" {
		processName := a.lastErrorProcess
		if processName == "" {
			processName = a.spec.Config.WebProcess
		}
		result.ErrorLog, _ = tail(filepath.Join(a.cfg.LogDir, a.spec.Name, processName+".log"), failureLogLines)
	}
	pids := make([]int, 0, len(a.processes))
	var earliest time.Time
	for _, p := range a.processes {
		result.Processes = append(result.Processes, ProcessSnapshot{Name: p.name, Command: p.command.Line, PID: p.pid, Port: p.port, Restarts: a.failures[p.name], StartedAt: p.startedAt})
		pids = append(pids, p.pid)
		if earliest.IsZero() || p.startedAt.Before(earliest) {
			earliest = p.startedAt
		}
	}
	sort.Slice(result.Processes, func(i, j int) bool { return result.Processes[i].Name < result.Processes[j].Name })
	if !earliest.IsZero() {
		result.Uptime = time.Since(earliest).Round(time.Second).String()
	}
	result.Resources, _ = a.backend.Stats(pids)
	return result
}

func (a *appRuntime) logs(processName string, lines int) (map[string][]string, error) {
	if lines <= 0 {
		lines = a.spec.Config.LogTailLines
	}
	result := map[string][]string{}
	for name := range a.spec.Commands {
		if processName != "" && name != processName {
			continue
		}
		values, err := tail(filepath.Join(a.cfg.LogDir, a.spec.Name, name+".log"), lines)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		result[name] = values
	}
	if processName != "" {
		if _, ok := result[processName]; !ok {
			return nil, fmt.Errorf("unknown process %q", processName)
		}
	}
	return result, nil
}

func tail(path string, count int) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > count {
			lines = lines[1:]
		}
	}
	return lines, scanner.Err()
}

func (a *appRuntime) cleanupProcess(name string) {
	p := a.processes[name]
	if p == nil {
		return
	}
	if p.log != nil {
		_ = p.log.Close()
	}
	close(p.done)
	delete(a.processes, name)
	a.removePID(name)
}

func (a *appRuntime) writePID(p *process) error {
	dir := filepath.Join(a.cfg.StateDir, a.spec.Name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	return os.WriteFile(a.pidPath(p.name), []byte(strconv.Itoa(p.pid)+"\n"), 0o640)
}

func (a *appRuntime) pidPath(name string) string {
	return filepath.Join(a.cfg.StateDir, a.spec.Name, name+".pid")
}
func (a *appRuntime) removePID(name string) { _ = os.Remove(a.pidPath(name)) }

func resolveExecutable(name, dir, pathValue string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		if filepath.IsAbs(name) {
			return name, nil
		}
		return filepath.Join(dir, name), nil
	}
	for _, pathDir := range filepath.SplitList(pathValue) {
		if !filepath.IsAbs(pathDir) {
			pathDir = filepath.Join(dir, pathDir)
		}
		candidate := filepath.Join(pathDir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in app PATH", name)
}

func (a *appRuntime) rotateLogs() {
	for name := range a.processes {
		defaults := a.spec.Config.Process(name)
		_ = rotateActiveLog(filepath.Join(a.cfg.LogDir, a.spec.Name, name+".log"), int64(defaults.LogMaxSize), defaults.LogKeep)
	}
}

func rotateActiveLog(path string, maximum int64, keep int) error {
	info, err := os.Stat(path)
	if err != nil || maximum <= 0 || info.Size() < maximum {
		return err
	}
	if keep <= 0 {
		return os.Truncate(path, 0)
	}
	shiftRotatedLogs(path, keep)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".1", data, 0o640); err != nil {
		return err
	}
	return os.Truncate(path, 0)
}

// shiftRotatedLogs drops the oldest archive and renames .i to .i+1, making room for a new .1.
func shiftRotatedLogs(path string, keep int) {
	_ = os.Remove(fmt.Sprintf("%s.%d", path, keep))
	for i := keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
	}
}

func alive(pid int) bool { err := syscall.Kill(pid, 0); return err == nil || err == syscall.EPERM }

func environment(spec *apps.App, processName string, port int, socket string, extra map[string]string) []string {
	values := map[string]string{}
	for key, value := range spec.Env {
		values[key] = value
	}
	for key, value := range extra {
		values[key] = value
	}
	values["PORT"] = strconv.Itoa(port)
	values["APP_NAME"] = spec.Name
	values["PROC_TYPE"] = processName
	values["APPBOSS_SOCKET"] = socket
	keys := slices.Sorted(maps.Keys(values))
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
