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
	requestProcessStart
	requestProcessStop
	requestProcessRestart
	requestRoll
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
	// done receives the outcome of a rolling restart once it finishes.
	done chan error
}
type response struct {
	snapshot    Snapshot
	logs        map[string][]string
	sealed      []string
	hooks       []HookSnapshot
	app         *apps.App
	err         error
	idleStopped bool
	rolled      bool
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
	name      string // the instance: "web", or "web.2" when the entry runs several
	proc      string // the procfile entry
	index     int    // 1-based instance number, PROC_INSTANCE
	slot      string // owns the port, cgroup and pid file
	cmd       *exec.Cmd
	pid       int
	port      int
	startedAt time.Time
	restarts  int
	oomBase   int64 // the domain's OOM kill count at spawn, so a reused cgroup's history is ignored
	released  bool
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
	routes      *routeTable
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
	host     func() config.Config
	sink     notify.Sink
	restart  func(string) error
	failures map[string]int
	ready    map[string]bool
	// held are the processes the operator stopped on their own; the restart policy leaves them
	// down until a process start or the next app start.
	held map[string]bool
	// retiring are processes on their way out after a rolling restart replaced them; they are
	// no longer routed and are released once they exit.
	retiring         map[*process]bool
	roll             *rollState
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
			a.publishRoutes()
			if req.reply != nil {
				req.reply <- result
			}
		case event := <-a.events:
			a.handleEvent(event)
			a.publishRoutes()
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
	case requestProcessStart:
		return response{err: a.startProcess(req.processName)}
	case requestProcessStop:
		return response{err: a.stopProcess(req.processName, true)}
	case requestProcessRestart:
		if err := a.stopProcess(req.processName, false); err != nil {
			return response{err: err}
		}
		return response{err: a.startProcess(req.processName)}
	case requestRoll:
		rolled, err := a.beginRoll(req.processName, req.done)
		return response{rolled: rolled, err: err}
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
	a.held = map[string]bool{}
	a.retiring = map[*process]bool{}
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
	if a.roll != nil && a.roll.step == state {
		a.rollStepDone(state, exitCode)
		return
	}
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
		for _, name := range a.liveInstances(web.Name) {
			a.watch(a.processes[name])
		}
	}
	return nil
}

// watch hands a web process to its readiness and liveness monitor; a worker needs none.
func (a *appRuntime) watch(p *process) {
	if p != nil && a.spec.Config.IsWeb(p.proc) {
		go a.monitor(p, a.spec.Config.Process(p.proc), a.webHost(p.proc), healthInterval)
	}
}

// startOrder lists the instances in the order start spawns them: every web process first, then the
// rest by name, so a web process that expects earlier setup still gets it. Port assignment is
// unaffected because assignPorts keeps its own name order.
func (a *appRuntime) startOrder() []string {
	names := slices.Sorted(maps.Keys(a.spec.Commands))
	ordered := make([]string, 0, len(names))
	for _, web := range []bool{true, false} {
		for _, name := range names {
			if a.spec.Config.IsWeb(name) == web {
				ordered = append(ordered, a.instancesOf(name)...)
			}
		}
	}
	return ordered
}

// allWebReady reports whether every web process has at least one instance past its readiness
// check, so the app flips to running once each of its hosts can be served.
func (a *appRuntime) allWebReady() bool {
	for _, web := range a.spec.Config.WebProcesses {
		if !a.webServing(web.Name) {
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

// spawnOne starts one instance in the first free slot of its process. A failure crashes the app
// through failStart, so a first start and a restart after an exit report it the same way.
func (a *appRuntime) spawnOne(name string) error {
	p, err := a.spawn(name, nil)
	if err != nil {
		a.lastErrorProcess = name
		return a.failStart(err)
	}
	a.processes[name] = p
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
	a.abortRoll(errors.New("the app was stopped"))
	err := a.stopProcesses()
	a.state = Stopped
	return err
}

// stopProcesses stops every live process and waits for the retiring ones a roll is draining, so
// nothing of the app outlives a stop.
func (a *appRuntime) stopProcesses() error {
	names := make([]string, 0, len(a.processes))
	for name := range a.processes {
		names = append(names, name)
	}
	err := a.killProcesses(names)
	for p := range a.retiring {
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		<-p.waited
		a.release(p)
	}
	return err
}

// killProcesses signals every named live instance, waits up to the longest stop_timeout and
// kills what is left, so one process and the whole app stop the same way.
func (a *appRuntime) killProcesses(names []string) error {
	var first error
	var victims []*process
	for _, name := range names {
		if p := a.processes[name]; p != nil {
			victims = append(victims, p)
		}
	}
	for _, p := range victims {
		signal, _ := res.Signal(a.spec.Config.Process(p.proc).StopSignal)
		if err := syscall.Kill(-p.pid, signal); err != nil && err != syscall.ESRCH && first == nil {
			first = err
		}
	}
	deadline := time.Now().Add(a.maxStopTimeout(victims))
	for _, p := range victims {
		if !p.waitUntil(deadline) {
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
			<-p.waited
		}
		a.release(p)
	}
	return first
}

// startProcess spawns the missing instances of one procfile process, or one named instance, of a
// live app. A running instance is left alone; a stopped app is started as a whole, never one
// process at a time.
func (a *appRuntime) startProcess(name string) error {
	names, err := a.resolve(name)
	if err != nil {
		return err
	}
	if a.state != Running && a.state != Starting {
		return fmt.Errorf("%s is %s; start the app first", a.spec.Name, a.state)
	}
	for _, instance := range names {
		delete(a.held, instance)
		if a.processes[instance] != nil {
			continue
		}
		a.failures[instance] = 0
		if err := a.spawnOne(instance); err != nil {
			return err
		}
		a.watch(a.processes[instance])
	}
	return nil
}

// stopProcess stops every instance of one process, or one named instance. hold keeps them down
// against the restart policy; a restart passes false because it spawns them again right away.
// The app reads as stopped once nothing of it is left running.
func (a *appRuntime) stopProcess(name string, hold bool) error {
	names, err := a.resolve(name)
	if err != nil {
		return err
	}
	for _, instance := range names {
		if hold {
			a.held[instance] = true
		}
		delete(a.ready, instance)
	}
	err = a.killProcesses(names)
	if hold && len(a.processes) == 0 && (a.state == Running || a.state == Starting) {
		a.state = Stopped
	}
	return err
}
