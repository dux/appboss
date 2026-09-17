package super

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"deploy-boss/internal/apps"
	"deploy-boss/internal/config"
	"deploy-boss/internal/ports"
	"deploy-boss/internal/res"
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
	Adopted   bool      `json:"adopted"`
}

type RequestRates struct {
	LastMinute int64 `json:"last_minute"`
	LastHour   int64 `json:"last_hour"`
	LastDay    int64 `json:"last_day"`
}

type Snapshot struct {
	Name         string            `json:"name"`
	State        State             `json:"state"`
	Hosts        []string          `json:"hosts"`
	WebProcess   string            `json:"web_process"`
	Processes    []ProcessSnapshot `json:"processes"`
	LastActivity time.Time         `json:"last_activity,omitempty"`
	Uptime       string            `json:"uptime,omitempty"`
	Resources    res.Stats         `json:"resources"`
	RequestRates RequestRates      `json:"request_rates"`
	Error        string            `json:"error,omitempty"`
	LogRetention time.Duration     `json:"-"`
	LogFlush     time.Duration     `json:"-"`
}

type Manager struct {
	cfg        config.Config
	ports      *ports.Allocator
	backend    res.Backend
	mu         sync.RWMutex
	apps       map[string]*appRuntime
	desiredMu  sync.Mutex
	desired    map[string]bool
	activities map[string]time.Time
	ctx        context.Context
	cancel     context.CancelFunc
}

func New(cfg config.Config, allocator *ports.Allocator) (*Manager, []error, error) {
	discovered, invalid, err := apps.Discover(cfg)
	if err != nil {
		return nil, invalid, err
	}
	desired, err := loadDesired(cfg.StateDir)
	if err != nil {
		return nil, invalid, err
	}
	activities, err := loadActivities(cfg.StateDir)
	if err != nil {
		return nil, invalid, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{cfg: cfg, ports: allocator, backend: res.Procgroup{}, apps: map[string]*appRuntime{}, desired: desired, activities: activities, ctx: ctx, cancel: cancel}
	for _, spec := range discovered {
		m.add(ctx, spec)
	}
	if cfg.Daemon.ResumeRunning {
		for name := range desired {
			if runtime := m.apps[name]; runtime != nil {
				_ = runtime.call(request{kind: requestStart, adopt: true})
			}
		}
	}
	go m.idleLoop(ctx)
	return m, invalid, nil
}

func (m *Manager) Close() {
	_ = m.saveActivities()
	m.cancel()
}

func (m *Manager) add(ctx context.Context, spec *apps.App) {
	runtimeCtx, cancel := context.WithCancel(ctx)
	runtime := &appRuntime{ctx: runtimeCtx, cancel: cancel, cfg: m.cfg, spec: spec, allocator: m.ports, backend: m.backend, requests: make(chan request), events: make(chan processEvent, 32), state: Stopped, processes: map[string]*process{}, failures: map[string]int{}}
	runtime.lastActivity = m.activities[spec.Name]
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

func (m *Manager) ReleasePorts(name string) error {
	snapshot, err := m.Snapshot(name)
	if err != nil {
		return err
	}
	return m.ports.Release(name, snapshot.State == Stopped)
}

func (m *Manager) Ports() map[string]int { return m.ports.Entries() }

func (m *Manager) Rescan() ([]error, error) {
	scanConfig := m.cfg
	if m.cfg.SourcePath != "" {
		loaded, err := config.Load(m.cfg.SourcePath)
		if err != nil {
			return nil, err
		}
		scanConfig.Apps = loaded.Apps
	}
	discovered, invalid, err := apps.Discover(scanConfig)
	if err != nil {
		return invalid, err
	}
	m.mu.Lock()
	m.cfg.Apps = append([]string(nil), scanConfig.Apps...)
	seen := map[string]bool{}
	for _, spec := range discovered {
		seen[spec.Name] = true
		if runtime := m.apps[spec.Name]; runtime != nil {
			runtime.call(request{kind: requestUpdate, spec: spec})
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
	names := make([]string, 0, len(m.desired))
	for app := range m.desired {
		names = append(names, app)
	}
	sort.Strings(names)
	data, _ := json.MarshalIndent(names, "", "  ")
	data = append(data, '\n')
	path := filepath.Join(m.cfg.StateDir, "running.json")
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

func loadDesired(stateDir string) (map[string]bool, error) {
	result := map[string]bool{}
	data, err := os.ReadFile(filepath.Join(stateDir, "running.json"))
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
	data, err := json.MarshalIndent(activities, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(m.cfg.StateDir, "last_activity.json")
	newPath := path + ".new"
	if err := os.WriteFile(newPath, data, 0o640); err != nil {
		return err
	}
	return os.Rename(newPath, path)
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
)

type request struct {
	kind        requestKind
	reply       chan response
	adopt       bool
	processName string
	lines       int
	now         time.Time
	spec        *apps.App
}
type response struct {
	snapshot    Snapshot
	logs        map[string][]string
	err         error
	idleStopped bool
}
type processEvent struct {
	kind      string
	name      string
	err       error
	exitCode  int
	startedAt time.Time
}
type process struct {
	name      string
	command   apps.Command
	cmd       *exec.Cmd
	pid       int
	port      int
	startedAt time.Time
	restarts  int
	adopted   bool
	log       io.WriteCloser
}

type appRuntime struct {
	ctx          context.Context
	cancel       context.CancelFunc
	cfg          config.Config
	spec         *apps.App
	allocator    *ports.Allocator
	backend      res.Backend
	requests     chan request
	events       chan processEvent
	state        State
	processes    map[string]*process
	failures     map[string]int
	lastActivity time.Time
	lastError    string
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
	logTicker := time.NewTicker(5 * time.Second)
	defer logTicker.Stop()
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
		case <-logTicker.C:
			a.rotateLogs()
		}
	}
}

func (a *appRuntime) handle(req request) response {
	switch req.kind {
	case requestStart:
		return response{err: a.start(req.adopt)}
	case requestStop:
		return response{err: a.stop()}
	case requestRestart:
		if err := a.stop(); err != nil {
			return response{err: err}
		}
		return response{err: a.start(false)}
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
	}
	return response{}
}

func (a *appRuntime) start(adopt bool) error {
	if a.state == Running || a.state == Starting {
		return nil
	}
	a.failures = map[string]int{}
	a.state, a.lastError = Starting, ""
	for name, command := range a.spec.Commands {
		port, hadPort := a.allocator.Lookup(a.spec.Name, name)
		var err error
		if !hadPort {
			port, err = a.allocator.Allocate(a.spec.Name, name)
		}
		if err != nil {
			return a.failStart(err)
		}
		var started bool
		if adopt && hadPort {
			started = a.adopt(name, command, port)
		}
		if !started {
			if err := a.spawn(name, command, port); err != nil {
				return a.failStart(err)
			}
		}
	}
	if _, hasWeb := a.spec.Commands[a.spec.Config.WebProcess]; !hasWeb {
		a.state = Running
		a.lastActivity = time.Now()
	} else {
		go a.readiness(a.spec.Config.WebProcess, a.processes[a.spec.Config.WebProcess].port)
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
	for len(a.processes) > 0 && time.Now().Before(deadline) {
		for name, p := range a.processes {
			if !alive(p.pid) {
				a.cleanupProcess(name)
			}
		}
		if len(a.processes) > 0 {
			time.Sleep(25 * time.Millisecond)
		}
	}
	for name, p := range a.processes {
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		a.cleanupProcess(name)
	}
	return first
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
	log, err := openProcessLog(filepath.Join(a.cfg.LogDir, a.spec.Name, name+".log"), int64(defaults.LogMaxSize), defaults.LogKeep)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return fmt.Errorf("start %s: %w", name, err)
	}
	if err := a.backend.Place(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = log.Close()
		return err
	}
	p := &process{name: name, command: command, cmd: cmd, pid: cmd.Process.Pid, port: port, startedAt: time.Now(), restarts: a.failures[name], log: log}
	a.processes[name] = p
	if err := a.writePID(p, defaults.Shell); err != nil {
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = log.Close()
		delete(a.processes, name)
		return err
	}
	go func() {
		err := cmd.Wait()
		exitCode := 0
		if err != nil {
			exitCode = -1
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
		}
		a.events <- processEvent{kind: "exit", name: name, err: err, exitCode: exitCode, startedAt: p.startedAt}
	}()
	return nil
}

func (a *appRuntime) adopt(name string, command apps.Command, port int) bool {
	pidPath := a.pidPath(name)
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil || !alive(pid) {
		a.removePID(name)
		return false
	}
	expectedData, err := os.ReadFile(strings.TrimSuffix(pidPath, ".pid") + ".cmd")
	if err != nil {
		a.removePID(name)
		return false
	}
	expected := strings.TrimSpace(string(expectedData))
	intended := commandString(command, a.spec.Config.Process(name).Shell)
	if !a.spec.Config.Process(name).Shell {
		resolved, resolveErr := resolveExecutable(command.Argv[0], a.spec.Dir, a.spec.Env["PATH"])
		if resolveErr != nil {
			a.removePID(name)
			return false
		}
		command.Argv = append([]string(nil), command.Argv...)
		command.Argv[0] = resolved
		intended = commandString(command, false)
	}
	actual, err := processCommand(pid)
	if err != nil || actual != expected || expected != intended {
		a.removePID(name)
		return false
	}
	p := &process{name: name, command: command, pid: pid, port: port, startedAt: time.Now(), adopted: true}
	a.processes[name] = p
	go func() {
		ticker := time.NewTicker(a.cfg.Daemon.AdoptPoll.Value())
		defer ticker.Stop()
		for range ticker.C {
			if !alive(pid) {
				a.events <- processEvent{kind: "exit", name: name, exitCode: -1, startedAt: p.startedAt}
				return
			}
		}
	}()
	return true
}

func (a *appRuntime) readiness(name string, port int) {
	defaults := a.spec.Config.Process(name)
	deadline := time.Now().Add(defaults.HealthTimeout.Value())
	ticker := time.NewTicker(defaults.HealthInterval.Value())
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if healthy(defaults.Health, port, a.cfg.Proxy.Upstream.DialTimeout.Value()) {
				a.events <- processEvent{kind: "ready", name: name}
				return
			}
			if time.Now().After(deadline) {
				a.events <- processEvent{kind: "health-failed", name: name, err: errors.New("readiness timeout")}
				return
			}
		}
	}
}

func healthy(check string, port int, timeout time.Duration) bool {
	address := fmt.Sprintf("127.0.0.1:%d", port)
	if check == "tcp" {
		connection, err := net.DialTimeout("tcp", address, timeout)
		if err == nil {
			_ = connection.Close()
			return true
		}
		return false
	}
	path := strings.TrimPrefix(check, "http:")
	client := &http.Client{Timeout: timeout}
	response, err := client.Get("http://" + address + path)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode >= 200 && response.StatusCode < 300
}

func (a *appRuntime) handleEvent(event processEvent) {
	switch event.kind {
	case "ready":
		if a.state == Starting {
			a.state = Running
			a.lastActivity = time.Now()
		}
	case "health-failed":
		a.lastError = event.err.Error()
		if p := a.processes[event.name]; p != nil {
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		}
	case "restart":
		if a.state != Running && a.state != Starting {
			return
		}
		command := a.spec.Commands[event.name]
		port, err := a.allocator.Allocate(a.spec.Name, event.name)
		if err == nil {
			err = a.spawn(event.name, command, port)
			if err == nil && event.name == a.spec.Config.WebProcess {
				go a.readiness(event.name, port)
			}
		}
		if err != nil {
			a.lastError = err.Error()
			_ = a.stopProcesses()
			a.state = Crashed
		}
	case "exit":
		a.processExited(event)
	}
}

func (a *appRuntime) processExited(event processEvent) {
	p := a.processes[event.name]
	if p == nil {
		return
	}
	a.cleanupProcess(event.name)
	if a.state == Stopping || a.state == Stopped {
		return
	}
	defaults := a.spec.Config.Process(event.name)
	if time.Since(event.startedAt) >= defaults.RestartReset.Value() {
		a.failures[event.name] = 0
	}
	shouldRestart := defaults.Restart == "always" || (defaults.Restart == "on-failure" && event.exitCode != 0)
	if event.name == a.spec.Config.WebProcess && shouldRestart {
		a.state = Starting
	}
	if !shouldRestart {
		if event.name == a.spec.Config.WebProcess {
			a.state = Crashed
			a.lastError = fmt.Sprintf("%s exited with code %d", event.name, event.exitCode)
		} else if len(a.processes) == 0 {
			a.state = Stopped
		}
		return
	}
	a.failures[event.name]++
	if a.failures[event.name] >= defaults.MaxRestarts {
		a.state, a.lastError = Crashed, fmt.Sprintf("%s exceeded max_restarts", event.name)
		_ = a.stopProcesses()
		a.state = Crashed
		return
	}
	delay := backoff(defaults.RestartBackoff, a.failures[event.name])
	time.AfterFunc(delay, func() {
		select {
		case a.events <- processEvent{kind: "restart", name: event.name}:
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
	result := Snapshot{Name: a.spec.Name, State: a.state, Hosts: a.spec.Config.Hosts, WebProcess: a.spec.Config.WebProcess, LastActivity: a.lastActivity, Error: a.lastError, LogRetention: a.spec.Config.LogRetention.Value(), LogFlush: a.spec.Config.LogFlush.Value()}
	pids := make([]int, 0, len(a.processes))
	var earliest time.Time
	for _, p := range a.processes {
		result.Processes = append(result.Processes, ProcessSnapshot{Name: p.name, Command: p.command.Line, PID: p.pid, Port: p.port, Restarts: a.failures[p.name], StartedAt: p.startedAt, Adopted: p.adopted})
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
		lines = a.cfg.Defaults.LogTailLines
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
	delete(a.processes, name)
	a.removePID(name)
}

func (a *appRuntime) writePID(p *process, shell bool) error {
	dir := filepath.Join(a.cfg.StateDir, a.spec.Name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(a.pidPath(p.name), []byte(strconv.Itoa(p.pid)+"\n"), 0o640); err != nil {
		return err
	}
	command := commandString(p.command, shell)
	return os.WriteFile(filepath.Join(dir, p.name+".cmd"), []byte(command+"\n"), 0o640)
}

func (a *appRuntime) pidPath(name string) string {
	return filepath.Join(a.cfg.StateDir, a.spec.Name, name+".pid")
}
func (a *appRuntime) removePID(name string) {
	_ = os.Remove(a.pidPath(name))
	_ = os.Remove(strings.TrimSuffix(a.pidPath(name), ".pid") + ".cmd")
}

func commandString(command apps.Command, shell bool) string {
	if shell {
		return strings.Join([]string{"/bin/sh", "-c", command.Line}, " ")
	}
	return strings.Join(command.Argv, " ")
}

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
	_ = os.Remove(fmt.Sprintf("%s.%d", path, keep))
	for i := keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".1", data, 0o640); err != nil {
		return err
	}
	return os.Truncate(path, 0)
}

func processCommand(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err == nil {
		return strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " ")), nil
	}
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	return strings.TrimSpace(string(output)), err
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
	values["BOSS_SOCKET"] = socket
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func openProcessLog(path string, maximum int64, keep int) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && maximum > 0 && info.Size() >= maximum {
		if keep > 0 {
			_ = os.Remove(fmt.Sprintf("%s.%d", path, keep))
			for i := keep - 1; i >= 1; i-- {
				_ = os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
			}
			_ = os.Rename(path, path+".1")
		} else {
			_ = os.Remove(path)
		}
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
}
