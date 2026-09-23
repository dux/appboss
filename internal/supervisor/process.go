package supervisor

import (
	"bufio"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/logx"
	"dboss/internal/notify"
	"dboss/internal/ports"
	"dboss/internal/res"
)

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
	env := processEnv(a.spec, name, port, a.cfg.Socket, defaults.Env)
	cmd, err := newCommand(a.spec.Dir, command, env)
	if err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	// The port is fixed for this process, so whatever holds it is stale and gets killed first.
	// A dev session's port came from the registry free and stays reserved, so anything on it
	// belongs to someone else and is left alone.
	if !a.cfg.Dev() {
		killed, err := ports.ClearPort(port, defaults.StopTimeout.Value())
		if err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}
		if len(killed) > 0 {
			logx.Warnf("%s/%s: killed pids %v holding port %d", a.spec.Name, name, killed, port)
		}
	}
	logFile, err := newLogWriter(filepath.Join(a.cfg.LogDir, a.spec.Name, name+".log"), logMaxSize, logKeep)
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
	backend := a.backendFor(name)
	limits := res.Limits{MemoryMax: int64(defaults.MemoryMax), CPUMax: defaults.CPUMax}
	if backend.Name() != "cgroup" && (limits.MemoryMax > 0 || limits.CPUMax > 0) {
		logx.Warnf("%s/%s: memory_max/cpu_max are ignored by the %s backend", a.spec.Name, name, backend.Name())
	}
	if err := backend.Place(a.spec.Name, name, cmd.Process.Pid, limits); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
		return err
	}
	p := &process{name: name, cmd: cmd, pid: cmd.Process.Pid, port: port, startedAt: time.Now(), restarts: a.failures[name], log: logFile, done: make(chan struct{}), waited: make(chan struct{})}
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
		a.sendEvent(processEvent{kind: "exit", proc: p, err: err, exitCode: exitCode(err)})
	}()
	return nil
}

func (a *appRuntime) handleEvent(event processEvent) {
	if event.job != nil {
		switch event.kind {
		case "job-exit":
			a.jobExited(event.job, event.exitCode, event.err)
		case "job-timeout":
			a.jobTimeout(event.job)
		}
		return
	}
	name := event.proc.name
	if event.kind != "restart" && a.processes[name] != event.proc {
		return
	}
	switch event.kind {
	case "ready":
		a.ready[name] = true
		if a.state == Starting && a.allWebReady() {
			a.state = Running
			a.lastActivity = time.Now()
		}
	case "health-failed":
		a.lastError = event.err.Error()
		a.lastErrorProcess = name
		a.emit(notify.HealthTimeout, a.lastError)
		_ = syscall.Kill(-event.proc.pid, syscall.SIGKILL)
	case "restart":
		if a.state != Running && a.state != Starting || a.processes[name] != nil || a.held[name] {
			return
		}
		if a.spawnOne(name) == nil && a.spec.Config.IsWeb(name) {
			go a.monitor(a.processes[name], a.spec.Config.Process(name), a.webHost(name))
		}
	case "exit":
		a.processExited(event)
	}
}

func (a *appRuntime) processExited(event processEvent) {
	name := event.proc.name
	a.cleanupProcess(name)
	delete(a.ready, name)
	if a.state == Stopping || a.state == Stopped || a.held[name] {
		return
	}
	defaults := a.spec.Config.Process(name)
	if time.Since(event.proc.startedAt) >= restartReset {
		a.failures[name] = 0
	}
	shouldRestart := defaults.Restart == "always" || (defaults.Restart == "on-failure" && event.exitCode != 0)
	if a.spec.Config.IsWeb(name) && shouldRestart {
		a.state = Starting
	}
	if !shouldRestart {
		if a.spec.Config.IsWeb(name) {
			a.state = Crashed
			a.lastError = fmt.Sprintf("%s exited with code %d", name, event.exitCode)
			a.lastErrorProcess = name
			a.emit(notify.Crash, a.lastError)
		} else if len(a.processes) == 0 {
			a.state = Stopped
		}
		return
	}
	a.failures[name]++
	if a.failures[name] >= defaults.MaxRestarts {
		a.lastErrorProcess = name
		_ = a.failStart(fmt.Errorf("%s exceeded max_restarts", name))
		return
	}
	if a.failures[name] >= 2 {
		a.emit(notify.RestartLoop, fmt.Sprintf("%s failed %d times in a row", name, a.failures[name]))
	}
	delay := backoff(a.failures[name])
	time.AfterFunc(delay, func() { a.sendEvent(processEvent{kind: "restart", proc: event.proc}) })
}

// Restart pacing: the delay starts at restartBackoffFirst, grows by restartBackoffFactor per
// consecutive failure up to restartBackoffMax, and the failure count resets once a process has
// stayed up for restartReset. The delays are vars so tests can restart at once.
const (
	restartBackoffFactor = 2.0
	restartReset         = time.Minute
)

var (
	restartBackoffFirst = time.Second
	restartBackoffMax   = 60 * time.Second
)

// Process log files rotate above logMaxSize and keep logKeep rotated files; dboss logs without -n
// prints logTailLines.
const (
	logMaxSize   = 10 << 20
	logKeep      = 5
	logTailLines = 500
)

func backoff(attempt int) time.Duration {
	delay := float64(restartBackoffFirst)
	for i := 1; i < attempt; i++ {
		delay *= restartBackoffFactor
	}
	if time.Duration(delay) > restartBackoffMax {
		return restartBackoffMax
	}
	return time.Duration(delay)
}

func (a *appRuntime) snapshot() Snapshot {
	result := Snapshot{Name: a.spec.Name, State: a.state, Maintenance: a.maintenance, Draining: a.draining, Dir: a.spec.Dir, Branch: a.spec.Branch, Pages: resolveDir(a.spec.Dir, a.spec.Config.Pages), Hosts: a.spec.Config.Hosts, WebProcesses: WebProcessSnapshots(a.spec.Config.WebProcesses), Autostart: a.spec.Config.Autostart.Starts(), Deletable: a.spec.Config.Deletable && a.cfg.App == nil, WakeButton: a.spec.Config.Autostart == config.AutostartButton, Web: a.spec.Config.Web, Cron: a.cronSnapshot(), Hooks: a.hookSnapshot(), LastActivity: a.lastActivity, Error: a.lastError, LogRetention: a.spec.Config.LogRetention.Value(), StdoutRetention: a.spec.Config.StdoutRetention.Value(), TmpClean: a.spec.Config.TmpClean.Value()}
	if result.Error != "" {
		processName := a.lastErrorProcess
		if processName == "" && len(a.spec.Config.WebProcesses) > 0 {
			processName = a.spec.Config.WebProcesses[0].Name
		}
		result.ErrorLog, _ = tail(filepath.Join(a.cfg.LogDir, a.spec.Name, processName+".log"), failureLogLines)
	}
	// Every procfile service is listed so the console can show the full set even while stopped;
	// only live processes carry a pid and count toward uptime and resource stats.
	total := res.Stats{Approximate: true}
	var earliest time.Time
	for _, name := range slices.Sorted(maps.Keys(a.spec.Commands)) {
		entry := ProcessSnapshot{Name: name, Command: a.spec.Commands[name].Line, State: Stopped, Restarts: a.failures[name]}
		if port, ok := a.allocator.Lookup(a.spec.Name, name); ok {
			entry.Port = port
		}
		if p := a.processes[name]; p != nil {
			entry.State = Running
			entry.PID, entry.Port, entry.StartedAt = p.pid, p.port, p.startedAt
			stats, _ := a.backendFor(name).Stats(a.spec.Name, name, []int{p.pid})
			entry.MemoryBytes = stats.MemoryBytes
			total.MemoryBytes += stats.MemoryBytes
			total.CPUPercent += stats.CPUPercent
			if earliest.IsZero() || p.startedAt.Before(earliest) {
				earliest = p.startedAt
			}
		}
		result.Processes = append(result.Processes, entry)
	}
	if !earliest.IsZero() {
		result.Uptime = time.Since(earliest).Round(time.Second).String()
	}
	result.Resources = total
	return result
}

func (a *appRuntime) logs(processName string, lines int) (map[string][]string, error) {
	if lines <= 0 {
		lines = logTailLines
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
	_ = a.backendFor(name).Release(a.spec.Name, name)
	close(p.done)
	delete(a.processes, name)
	a.removePID(name)
}

// backendFor picks the resource backend of one process: cgroup unless cgroups are unavailable on
// this host.
func (a *appRuntime) backendFor(string) res.Backend {
	if a.cgroup != nil {
		return a.cgroup
	}
	return a.backend
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

// resolveDir resolves a path from the app file against the app folder.
func resolveDir(dir, value string) string {
	if value == "" || filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(dir, value)
}
