package supervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

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

func (a *appRuntime) maxStopTimeout(processes []*process) time.Duration {
	maximum := time.Duration(0)
	for _, p := range processes {
		if d := a.spec.Config.Process(p.proc).StopTimeout.Value(); d > maximum {
			maximum = d
		}
	}
	return maximum
}

// spawn starts one instance in the first free slot of its process and returns it untracked: the
// caller decides whether it takes the instance's place now or after a readiness check. log is the
// writer of a copy that is still running, so both write one file; nil opens the instance's own.
func (a *appRuntime) spawn(name string, log *logWriter) (*process, error) {
	proc, index := splitInstance(name)
	command, ok := a.spec.Commands[proc]
	if !ok {
		return nil, fmt.Errorf("unknown process %q", proc)
	}
	slot := a.freeSlot(proc)
	port, err := a.allocator.Allocate(a.spec.Name, slot)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	defaults := a.spec.Config.Process(proc)
	env := processEnv(a.spec, proc, port, a.cfg.Socket, a.cfg.LogDir, defaults.Env)
	env["PROC_INSTANCE"] = strconv.Itoa(index)
	cmd, err := newCommand(a.spec.Dir, command, env)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	// The port is fixed for this slot and no tracked process holds it, so whatever does is stale
	// and gets killed first. A dev session's port came from the registry free and stays
	// reserved, so anything on it belongs to someone else and is left alone.
	if !a.cfg.Dev() {
		killed, err := ports.ClearPort(port, defaults.StopTimeout.Value())
		if err != nil {
			return nil, fmt.Errorf("start %s: %w", name, err)
		}
		if len(killed) > 0 {
			logx.Warnf("%s/%s: killed pids %v holding port %d", a.spec.Name, name, killed, port)
		}
	}
	logFile := log
	if logFile == nil {
		if logFile, err = newLogWriter(filepath.Join(a.cfg.LogDir, a.spec.Name, name+".log"), logMaxSize, logKeep); err != nil {
			return nil, err
		}
		if a.echo != nil {
			logFile.echo = a.echo.writer(a.spec.Name, name)
		}
	}
	// Only a writer opened here is closed on failure; a shared one belongs to the running copy.
	closeLog := func() {
		if log == nil {
			_ = logFile.Close()
		}
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		closeLog()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	backend := a.backendFor(proc)
	limits := res.Limits{MemoryMax: int64(defaults.MemoryMax), CPUMax: defaults.CPUMax}
	if backend.Name() != "cgroup" && (limits.MemoryMax > 0 || limits.CPUMax > 0) {
		logx.Warnf("%s/%s: memory_max/cpu_max are ignored by the %s backend", a.spec.Name, name, backend.Name())
	}
	if err := backend.Place(a.spec.Name, slot, cmd.Process.Pid, limits); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		closeLog()
		return nil, err
	}
	p := &process{name: name, proc: proc, index: index, slot: slot, cmd: cmd, pid: cmd.Process.Pid, port: port, startedAt: time.Now(), restarts: a.failures[name], oomBase: backend.OOMKills(a.spec.Name, slot), log: logFile, done: make(chan struct{}), waited: make(chan struct{})}
	if err := a.writePID(p); err != nil {
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		_ = cmd.Wait()
		closeLog()
		return nil, err
	}
	go func() {
		err := cmd.Wait()
		close(p.waited)
		a.sendEvent(processEvent{kind: "exit", proc: p, err: err, exitCode: exitCode(err)})
	}()
	return p, nil
}

func (a *appRuntime) handleEvent(event processEvent) {
	if event.job != nil {
		switch event.kind {
		case "job-exit":
			a.jobExited(event.job, event.exitCode, event.err)
		case "job-timeout":
			a.jobTimeout(event.job)
		case "hook-restarted":
			a.hookRestarted(event.job, event.err)
		}
		return
	}
	if event.kind == "retired" {
		a.retired(event.proc)
		return
	}
	if a.roll != nil && a.roll.current == event.proc {
		a.rollEvent(event)
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
		if a.state != Running && a.state != Starting || a.processes[name] != nil || a.held[name] || !a.wanted(name) {
			return
		}
		if a.spawnOne(name) == nil {
			a.watch(a.processes[name])
		}
	case "exit":
		a.processExited(event)
	}
}

func (a *appRuntime) processExited(event processEvent) {
	p := event.proc
	name := p.name
	cause := a.oomCause(p)
	a.release(p)
	delete(a.ready, name)
	if a.state == Stopping || a.state == Stopped || a.held[name] {
		return
	}
	defaults := a.spec.Config.Process(p.proc)
	if time.Since(p.startedAt) >= restartReset {
		a.failures[name] = 0
	}
	shouldRestart := defaults.Restart == "always" || (defaults.Restart == "on-failure" && event.exitCode != 0)
	// Another ready copy keeps the process's hosts served, so the app only drops back to starting
	// or crashed when the last one is gone.
	web := a.spec.Config.IsWeb(p.proc)
	lastCopy := web && !a.webServing(p.proc)
	if lastCopy && shouldRestart {
		a.state = Starting
	}
	if !shouldRestart {
		if web {
			if lastCopy {
				a.state = Crashed
			}
			a.lastError = fmt.Sprintf("%s exited with code %d%s", name, event.exitCode, cause)
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
		_ = a.failStart(fmt.Errorf("%s exceeded max_restarts%s", name, cause))
		return
	}
	if a.failures[name] >= 2 {
		a.emit(notify.RestartLoop, fmt.Sprintf("%s failed %d times in a row%s", name, a.failures[name], cause))
	}
	delay := backoff(a.failures[name])
	time.AfterFunc(delay, func() { a.sendEvent(processEvent{kind: "restart", proc: p}) })
}

// oomCause reports whether the kernel OOM killer ended p, logging it into the process log and
// notifying. It reads the cgroup, so it runs before release removes it. The result is the suffix
// every crash message about this exit carries, empty when memory was not the cause.
func (a *appRuntime) oomCause(p *process) string {
	if a.backendFor(p.proc).OOMKills(a.spec.Name, p.slot) <= p.oomBase {
		return ""
	}
	cause, message := " (out of memory)", p.name+" was killed by the kernel OOM killer"
	if limit := a.spec.Config.Process(p.proc).MemoryMax; limit > 0 {
		cause = fmt.Sprintf(" (out of memory, memory_max %s)", limit)
		message += fmt.Sprintf(" at memory_max %s", limit)
	}
	if p.log != nil {
		line, _ := json.Marshal(map[string]string{"level": "error", "event": "oom", "message": message})
		_, _ = p.log.Write(append(line, '\n'))
	}
	logx.Warnf("%s/%s", a.spec.Name, message)
	a.emit(notify.OOM, message)
	return cause
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
	result := Snapshot{Name: a.spec.Name, State: a.state, Maintenance: a.maintenance, Draining: a.draining, Rolling: a.roll != nil, Dir: a.spec.Dir, Branch: a.spec.Branch, BranchURL: a.spec.BranchURL, Pages: resolveDir(a.spec.Dir, a.spec.Config.Pages), Hosts: a.spec.Config.Hosts, WebProcesses: WebProcessSnapshots(a.spec.Config.WebProcesses), Autostart: a.spec.Config.Autostart.Starts(), Deletable: a.spec.Config.Deletable && a.cfg.App == nil, WakeButton: a.spec.Config.Autostart == config.AutostartButton, Web: a.spec.Config.Web, Cron: a.cronSnapshot(), Hooks: a.hookSnapshot(), LastActivity: a.lastActivity, Error: a.lastError, LogRetention: a.spec.Config.LogRetention.Value(), StdoutRetention: a.spec.Config.StdoutRetention.Value(), TmpClean: a.spec.Config.TmpClean.Value()}
	if result.Error != "" {
		processName := a.lastErrorProcess
		if processName == "" && len(a.spec.Config.WebProcesses) > 0 {
			processName = a.instancesOf(a.spec.Config.WebProcesses[0].Name)[0]
		}
		result.ErrorLog, _ = tail(filepath.Join(a.cfg.LogDir, a.spec.Name, processName+".log"), failureLogLines)
	}
	// Every instance is listed so the console can show the full set even while stopped; only
	// live processes carry a pid and count toward uptime and resource stats.
	total := res.Stats{Approximate: true}
	var earliest time.Time
	for _, name := range a.knownInstances() {
		proc, index := splitInstance(name)
		entry := ProcessSnapshot{Name: name, Type: proc, Command: a.spec.Commands[proc].Line, State: Stopped, Restarts: a.failures[name]}
		if port, ok := a.allocator.Lookup(a.spec.Name, slotName(proc, index-1)); ok {
			entry.Port = port
		}
		if p := a.processes[name]; p != nil {
			entry.State = Running
			entry.PID, entry.Port, entry.StartedAt = p.pid, p.port, p.startedAt
			stats, _ := a.backendFor(proc).Stats(a.spec.Name, p.slot, []int{p.pid})
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

// logs tails the log of every instance, or of the instances of one process or one instance.
func (a *appRuntime) logs(processName string, lines int) (map[string][]string, error) {
	if lines <= 0 {
		lines = logTailLines
	}
	names := a.knownInstances()
	if processName != "" {
		var err error
		if names, err = a.resolve(processName); err != nil {
			return nil, err
		}
	}
	result := map[string][]string{}
	for _, name := range names {
		values, err := tail(filepath.Join(a.cfg.LogDir, a.spec.Name, name+".log"), lines)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		result[name] = values
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

// release forgets a process that has exited: its slot's cgroup and pid file go, and its log is
// closed unless the copy that replaced it, or the one it replaces, still writes there.
func (a *appRuntime) release(p *process) {
	if p.released {
		return
	}
	p.released = true
	if a.processes[p.name] == p {
		delete(a.processes, p.name)
	}
	delete(a.retiring, p)
	if a.roll != nil && a.roll.current == p {
		a.roll.current = nil
	}
	if p.log != nil && !a.logShared(p) {
		_ = p.log.Close()
	}
	_ = a.backendFor(p.proc).Release(a.spec.Name, p.slot)
	close(p.done)
	a.removePID(p.slot)
}

// logShared reports whether another tracked process writes through p's log.
func (a *appRuntime) logShared(p *process) bool {
	for _, other := range a.tracked() {
		if other != p && other.log == p.log {
			return true
		}
	}
	return false
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
	return os.WriteFile(a.pidPath(p.slot), []byte(strconv.Itoa(p.pid)+"\n"), 0o640)
}

func (a *appRuntime) pidPath(slot string) string {
	return filepath.Join(a.cfg.StateDir, a.spec.Name, slot+".pid")
}
func (a *appRuntime) removePID(slot string) { _ = os.Remove(a.pidPath(slot)) }

// resolveDir resolves a path from the app file against the app folder.
func resolveDir(dir, value string) string {
	if value == "" || filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(dir, value)
}
