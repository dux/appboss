package supervisor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/logx"
	"dboss/internal/notify"
	"dboss/internal/ports"
	"dboss/internal/res"
)

const failureLogLines = 1000

type Manager struct {
	cfg             config.Config
	hostConfig      config.Config
	ports           *ports.Allocator
	backend         res.Backend
	cgroup          res.Backend
	echo            *Echo
	sink            notify.Sink
	closeOnce       sync.Once
	rescanMu        sync.Mutex
	mu              sync.RWMutex
	apps            map[string]*appRuntime
	restartRequired []string
	desiredMu       sync.Mutex
	desired         map[string]bool
	maintenance     map[string]bool
	created         map[string]bool
	activities      map[string]time.Time
	inflightMu      sync.Mutex
	inflight        map[string]*atomic.Int64
	ctx             context.Context
	cancel          context.CancelFunc
	bootOnce        sync.Once
	booted          atomic.Bool
}

// New discovers the apps and assigns their ports; nothing starts before Boot.
// A non-nil echo mirrors every process's output to it, which the foreground session uses when
// attached to a terminal. An optional sink receives crash and failure events.
func New(cfg config.Config, allocator *ports.Allocator, echo *Echo, sinks ...notify.Sink) (*Manager, []error, error) {
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
	created, err := loadNames(filepath.Join(cfg.StateDir, "created.json"))
	if err != nil {
		return nil, invalid, err
	}
	activities, err := loadActivities(cfg.StateDir)
	if err != nil {
		return nil, invalid, err
	}
	var sink notify.Sink
	if len(sinks) > 0 {
		sink = sinks[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{cfg: cfg, hostConfig: cfg, ports: allocator, backend: res.Procgroup{}, cgroup: selectCgroup(), echo: echo, sink: sink, apps: map[string]*appRuntime{}, desired: desired, maintenance: maintenance, created: created, activities: activities, inflight: map[string]*atomic.Int64{}, ctx: ctx, cancel: cancel}
	for _, spec := range discovered {
		if err := m.assignPorts(spec); err != nil {
			cancel()
			return nil, invalid, err
		}
		m.add(ctx, spec)
	}
	return m, invalid, nil
}

// Boot starts the apps that were running before, skipping autostart: false, and the idle and
// cron loops. Until then the session only serves: a request gets the waiting page instead of
// waking its app, so a hand-run start can print every address before anything loads.
func (m *Manager) Boot() {
	m.bootOnce.Do(func() {
		m.booted.Store(true)
		m.desiredMu.Lock()
		names := slices.Sorted(maps.Keys(m.desired))
		m.desiredMu.Unlock()
		for _, name := range names {
			if runtime, err := m.runtime(name); err == nil && runtime.spec.Config.Autostart.Starts() {
				_ = runtime.call(request{kind: requestStart})
			}
		}
		go m.idleLoop(m.ctx)
		go m.cronLoop(m.ctx)
	})
}

func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		_ = m.saveActivities()
		runtimes := m.runtimes()
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

// selectCgroup returns the cgroup backend when the host can use it. A box without a writable
// cgroup v2 hierarchy gets nil and the procgroup fallback.
func selectCgroup() res.Backend {
	if !res.Available(res.DefaultCgroupRoot) {
		return nil
	}
	return res.NewCgroup(res.DefaultCgroupRoot)
}

func (m *Manager) add(ctx context.Context, spec *apps.App) {
	runtimeCtx, cancel := context.WithCancel(ctx)
	runtime := &appRuntime{ctx: runtimeCtx, cancel: cancel, cfg: m.cfg, spec: spec, allocator: m.ports, backend: m.backend, cgroup: m.cgroup, echo: m.echo, host: m.HostConfig, sink: m.sink, restart: m.Restart, markCreated: m.markCreated, requests: make(chan request), events: make(chan processEvent, 32), state: Stopped, processes: map[string]*process{}, failures: map[string]int{}, held: map[string]bool{}, cron: map[string]*jobState{}, hooks: map[string]*jobState{}, lifecycle: map[string]*jobState{}, closed: make(chan struct{})}
	runtime.lastActivity = m.activities[spec.Name]
	runtime.maintenance = m.maintenance[spec.Name]
	m.desiredMu.Lock()
	runtime.created = m.created[spec.Name]
	m.desiredMu.Unlock()
	runtime.syncCron(time.Now())
	runtime.syncHooks()
	runtime.syncLifecycle()
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

// StartProcess, StopProcess and RestartProcess act on one procfile process of a live app. A
// stopped process stays down until it is started again or the whole app starts; none of them
// changes whether the app itself is desired.
func (m *Manager) StartProcess(name, process string) error {
	return m.processCall(name, process, requestProcessStart)
}

func (m *Manager) StopProcess(name, process string) error {
	return m.processCall(name, process, requestProcessStop)
}

func (m *Manager) RestartProcess(name, process string) error {
	return m.processCall(name, process, requestProcessRestart)
}

func (m *Manager) processCall(name, process string, kind requestKind) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	return runtime.call(request{kind: kind, processName: process})
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

	m.mu.Lock()
	delete(m.apps, name)
	delete(m.activities, name)
	m.mu.Unlock()
	m.inflightMu.Lock()
	delete(m.inflight, name)
	m.inflightMu.Unlock()
	runtime.cancel()
	<-runtime.closed
	m.runDestroyStep(runtime.spec)
	_ = os.Remove(filepath.Join(m.cfg.StateDir, name))
	if err := apps.Destroy(m.cfg.Apps, name); err != nil {
		return fmt.Errorf("destroy %s: %w", name, err)
	}
	return nil
}

// Wake starts an app on behalf of the proxy and reports a failed start, which an explicit run or
// console start does not. Before Boot it does nothing, so a tab opened early waits on the
// starting page instead of loading the app ahead of the rest.
// Booted reports whether Boot ran; before it a hand-run session is held at its ENTER prompt.
func (m *Manager) Booted() bool { return m.booted.Load() }

func (m *Manager) Wake(name string) {
	if !m.booted.Load() {
		return
	}
	if err := m.Start(name); err != nil {
		logx.Warnf("wake %s: %v", name, err)
		m.emit(notify.Event{Type: notify.WakeFailed, App: name, Error: err.Error(), Time: time.Now()})
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

// RestartRequired lists the host keys whose value on disk differs from the running session.
func (m *Manager) RestartRequired() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.restartRequired...)
}

// runtimes copies the current runtimes, so the caller can query them without holding m.mu.
func (m *Manager) runtimes() []*appRuntime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Collect(maps.Values(m.apps))
}

func (m *Manager) Snapshots() []Snapshot {
	runtimes := m.runtimes()
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
		if score, ok := config.BestMatch(host, snapshot.Hosts); ok && score > bestScore {
			best, bestScore = snapshot, score
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
		// The console may have created the server-only override since start; it wins from now on.
		current, err := config.Load(config.Live(m.cfg.SourcePath))
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
		// The console port is picked at start, not read from the file.
		loaded.ConsolePort = m.hostConfig.ConsolePort
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
			runtimes := m.runtimes()
			for _, runtime := range runtimes {
				_ = runtime.call(request{kind: requestCron, now: now})
			}
		}
	}
}

// idleTick is how often idle_stop is checked; a var so tests can run it faster.
var idleTick = time.Minute

func (m *Manager) idleLoop(ctx context.Context) {
	ticker := time.NewTicker(idleTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			runtimes := m.runtimes()
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
