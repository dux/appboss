package super

import (
	"bufio"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/logx"
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
	var cmd *exec.Cmd
	if defaults.Shell {
		cmd = exec.Command("/bin/sh", "-c", command.Line)
	} else {
		resolved, err := resolveExecutable(command.Argv[0], a.spec.Dir, env["PATH"])
		if err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}
		command.Argv = append([]string(nil), command.Argv...)
		command.Argv[0] = resolved
		cmd = exec.Command(resolved, command.Argv[1:]...)
	}
	cmd.Dir = a.spec.Dir
	cmd.Env = envSlice(env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The port is fixed for this process, so whatever holds it is stale and gets killed first.
	killed, err := clearPort(port, defaults.StopTimeout.Value())
	if err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	if len(killed) > 0 {
		logx.Warnf("%s/%s: killed pids %v holding port %d", a.spec.Name, name, killed, port)
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

// monitor walks the web process from spawn to exit. It first polls until the process answers
// (readiness, bounded by health_timeout), then keeps polling and reports a health failure after
// unhealthy_threshold consecutive failures. The runtime handles that exactly like a crash, so
// restart policy, backoff and max_restarts apply. unhealthy_threshold: 0 stops after readiness.
// It is given the process defaults and host instead of reading the app spec, which a rescan may
// replace on the runtime goroutine.
func (a *appRuntime) monitor(p *process, defaults config.Process, host string) {
	deadline := time.Now().Add(defaults.HealthTimeout.Value())
	ticker := time.NewTicker(defaults.HealthInterval.Value())
	defer ticker.Stop()
	ready := false
	failures := 0
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
				if !ready {
					ready = true
					a.sendEvent(processEvent{kind: "ready", proc: p})
					if defaults.UnhealthyThreshold <= 0 {
						return
					}
				}
				failures = 0
				continue
			}
			if err != nil {
				lastError = err
			}
			if !ready {
				if time.Now().After(deadline) {
					a.sendEvent(processEvent{kind: "health-failed", proc: p, err: lastError})
					return
				}
				continue
			}
			failures++
			if failures >= defaults.UnhealthyThreshold {
				a.sendEvent(processEvent{kind: "health-failed", proc: p, err: fmt.Errorf("unhealthy after %d failed checks: %w", failures, lastError)})
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
		return false, fmt.Errorf("healthcheck on tcp failed: %w", err)
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
		return false, fmt.Errorf("healthcheck on %s failed: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("healthcheck on %s returned %d", path, response.StatusCode)
	}
	return true, nil
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
		if a.state == Starting {
			a.state = Running
			a.lastActivity = time.Now()
		}
	case "health-failed":
		a.lastError = event.err.Error()
		a.lastErrorProcess = name
		a.emit("health-timeout", a.lastError)
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
				go a.monitor(a.processes[name], a.spec.Config.Process(name), a.webHost())
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
			a.emit("crash", a.lastError)
		} else if len(a.processes) == 0 {
			a.state = Stopped
		}
		return
	}
	a.failures[name]++
	if a.failures[name] >= defaults.MaxRestarts {
		a.state, a.lastError = Crashed, fmt.Sprintf("%s exceeded max_restarts", name)
		a.lastErrorProcess = name
		a.emit("crash", a.lastError)
		_ = a.stopProcesses()
		a.state = Crashed
		return
	}
	if a.failures[name] >= 2 {
		a.emit("restart-loop", fmt.Sprintf("%s failed %d times in a row", name, a.failures[name]))
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
	result := Snapshot{Name: a.spec.Name, State: a.state, Maintenance: a.maintenance, Draining: a.draining, Dir: a.spec.Dir, Hosts: a.spec.Config.Hosts, CanonicalHost: a.spec.Config.CanonicalHost, WebProcess: a.spec.Config.WebProcess, Autostart: a.spec.Config.Autostart.Starts(), Deletable: a.spec.Config.Deletable && a.cfg.App == nil, WakeButton: a.spec.Config.Autostart == config.AutostartButton, Web: a.spec.Config.Web, Cron: a.cronSnapshot(), Hooks: a.hookSnapshot(), LastActivity: a.lastActivity, Error: a.lastError, LogRetention: a.spec.Config.LogRetention.Value(), StdoutRetention: a.spec.Config.StdoutRetention.Value(), LogFlush: a.spec.Config.LogFlush.Value()}
	if result.Error != "" {
		processName := a.lastErrorProcess
		if processName == "" {
			processName = a.spec.Config.WebProcess
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
	_ = a.backendFor(name).Release(a.spec.Name, name)
	close(p.done)
	delete(a.processes, name)
	a.removePID(name)
}

// backendFor picks the resource backend of one process: cgroup unless the process asked for
// procgroup or cgroups are unavailable on this host.
func (a *appRuntime) backendFor(process string) res.Backend {
	if a.cgroup != nil && a.spec.Config.Process(process).Resources != "procgroup" {
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

// shiftRotatedLogs drops the oldest archive and renames .i to .i+1, making room for a new .1.
func shiftRotatedLogs(path string, keep int) {
	_ = os.Remove(fmt.Sprintf("%s.%d", path, keep))
	for i := keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
	}
}

func alive(pid int) bool { err := syscall.Kill(pid, 0); return err == nil || err == syscall.EPERM }

// processEnv assembles one process environment in the documented priority order, lowest first:
// the daemon environment and mise (spec.Env), then config env (extra, including a process
// override), then .env and .env.local, then the values dboss injects.
func processEnv(spec *apps.App, processName string, port int, socket string, extra map[string]string) map[string]string {
	values := map[string]string{}
	for key, value := range spec.Env {
		values[key] = value
	}
	for key, value := range extra {
		values[key] = value
	}
	for key, value := range spec.FileEnv {
		values[key] = value
	}
	// Cron jobs run outside the port table, so port 0 means no PORT is injected.
	if port > 0 {
		values["PORT"] = strconv.Itoa(port)
	}
	values["APP_NAME"] = spec.Name
	values["PROC_TYPE"] = processName
	values["DBOSS_SOCKET"] = socket
	return values
}

func envSlice(values map[string]string) []string {
	keys := slices.Sorted(maps.Keys(values))
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
