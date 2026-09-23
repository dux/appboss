package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/notify"
	"dboss/internal/ports"
	"dboss/internal/res"
)

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
	hooks       []HookSnapshot
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
	ctx         context.Context
	cancel      context.CancelFunc
	cfg         config.Config
	spec        *apps.App
	allocator   *ports.Allocator
	backend     res.Backend
	cgroup      res.Backend
	echo        *Echo
	requests    chan request
	events      chan processEvent
	state       State
	processes   map[string]*process
	cron        map[string]*jobState
	hooks       map[string]*jobState
	lifecycle   map[string]*jobState
	created     bool
	markCreated func(string)
	// host is the live host config, for the keys a rescan may change (tokens).
	host             func() config.Config
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
		a.syncLifecycle()
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
		return response{hooks: a.hookSnapshot()}
	case requestExecInfo:
		return response{app: a.spec}
	}
	return response{}
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
	return a.continueStart("")
}

// continueStart runs the lifecycle steps still due after the step named after ("" before the
// first), one at a time: jobExited calls back in when a step exits. With none left it spawns.
// create runs only until it has succeeded once for this app.
func (a *appRuntime) continueStart(after string) error {
	steps := []string{"create", "start"}
	if after != "" {
		steps = steps[slices.Index(steps, after)+1:]
	}
	for _, step := range steps {
		state := a.lifecycle[step]
		if state == nil || step == "create" && a.created {
			continue
		}
		if err := a.startJob(state, time.Now(), true); err != nil {
			a.lastErrorProcess = state.channel
			return a.failStart(fmt.Errorf("lifecycle %s: %w", step, err))
		}
		return nil
	}
	return a.spawnAll()
}

// stepExited moves a start along once its create or start step exits. A step whose start was
// cancelled meanwhile is ignored: stop kills the run before the app leaves Starting.
func (a *appRuntime) stepExited(state *jobState, exitCode int) {
	if a.state != Starting {
		return
	}
	if exitCode != 0 {
		a.lastErrorProcess = state.channel
		_ = a.failStart(fmt.Errorf("lifecycle %s: %s", state.name, state.lastError))
		return
	}
	if state.name == "create" {
		a.created = true
		if a.markCreated != nil {
			a.markCreated(a.spec.Name)
		}
	}
	_ = a.continueStart(state.name)
}

// spawnAll starts every process, web processes first, and hands the web ones to their monitor.
func (a *appRuntime) spawnAll() error {
	for _, name := range a.startOrder() {
		if err := a.spawnOne(name); err != nil {
			return err
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
		if web.Name == name {
			return config.PrimaryHost(web.CanonicalHost, web.Hosts)
		}
	}
	return ""
}

// spawnOne starts one process on its fixed port. A failure crashes the app through failStart, so
// a first start and a restart after an exit report it the same way.
func (a *appRuntime) spawnOne(name string) error {
	port, err := a.allocator.Allocate(a.spec.Name, name)
	if err == nil {
		err = a.spawn(name, a.spec.Commands[name], port)
	}
	if err != nil {
		a.lastErrorProcess = name
		return a.failStart(err)
	}
	return nil
}

func (a *appRuntime) failStart(err error) error {
	a.lastError = err.Error()
	_ = a.stopProcesses()
	a.state = Crashed
	a.emit(notify.Crash, a.lastError)
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
	a.stopSteps()
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
