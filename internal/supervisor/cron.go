package supervisor

import (
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"syscall"
	"time"

	"dboss/internal/apps"
	"dboss/internal/git"
	"dboss/internal/logx"
	"dboss/internal/notify"
	"dboss/internal/schedule"
)

// cronTick is how often the manager asks each app for due jobs. It bounds how late an "every"
// interval or cron expression can fire.
const cronTick = 15 * time.Second

// CronSnapshot is the live state of one scheduled job, carried in the app snapshot.
type CronSnapshot struct {
	Name      string    `json:"name"`
	Schedule  string    `json:"schedule"`
	Command   string    `json:"command"`
	Disabled  bool      `json:"disabled,omitempty"`
	Next      time.Time `json:"next,omitempty"`
	LastStart time.Time `json:"last_start,omitempty"`
	LastEnd   time.Time `json:"last_end,omitempty"`
	LastExit  int       `json:"last_exit"`
	LastError string    `json:"last_error,omitempty"`
	Running   bool      `json:"running"`
}

// HookSnapshot is the live state of one deploy hook, carried in the app snapshot. The secret is
// deliberately absent; only the dedicated hooks view returns it.
type HookSnapshot struct {
	Name      string    `json:"name"`
	Command   string    `json:"command"`
	Restart   bool      `json:"restart,omitempty"`
	Disabled  bool      `json:"disabled,omitempty"`
	LastStart time.Time `json:"last_start,omitempty"`
	LastEnd   time.Time `json:"last_end,omitempty"`
	LastExit  int       `json:"last_exit"`
	LastError string    `json:"last_error,omitempty"`
	Running   bool      `json:"running"`
}

// jobState is one one-shot command: a scheduled cron job (schedule set) or a hook (restart set).
// runs holds the executions currently going; with overlap false there is at most one.
type jobState struct {
	kind      string
	name      string
	channel   string
	command   apps.Command
	schedule  schedule.Schedule
	timeout   time.Duration
	overlap   bool
	disabled  bool
	restart   bool
	pull      bool
	next      time.Time
	runs      map[*jobRun]bool
	lastStart time.Time
	lastEnd   time.Time
	lastExit  int
	lastError string
	log       *logWriter
}

// jobRun is one execution of a job. It owns its command and wait channel; the log writer is
// shared with the job so a hook and a cron tick never step on each other's output.
type jobRun struct {
	state     *jobState
	cmd       *exec.Cmd
	startedAt time.Time
	waited    chan struct{}
	log       *logWriter
	timeout   *time.Timer
	finished  atomic.Bool
}

// cronName and hookName are the log channel and file basename of a job.
func cronName(job string) string  { return "cron-" + job }
func hookName(name string) string { return "hook-" + name }
func stepName(step string) string { return "lifecycle-" + step }

// syncCron reconciles the runtime's schedule with the current app spec: new jobs are added, a
// changed schedule gets a new next run, removed jobs are killed and dropped.
func (a *appRuntime) syncCron(now time.Time) {
	if a.cron == nil {
		a.cron = map[string]*jobState{}
	}
	for name, job := range a.spec.Cron {
		state := a.cron[name]
		if state == nil {
			a.cron[name] = &jobState{kind: "cron", name: name, channel: cronName(name), command: job.Command, schedule: job.Schedule, timeout: job.Timeout, overlap: job.Overlap, disabled: job.Disabled, next: job.Schedule.Next(now), runs: map[*jobRun]bool{}}
			continue
		}
		if state.schedule.String() != job.Schedule.String() {
			state.next = job.Schedule.Next(now)
		}
		state.command, state.schedule, state.timeout, state.overlap, state.disabled = job.Command, job.Schedule, job.Timeout, job.Overlap, job.Disabled
	}
	for name, state := range a.cron {
		if _, ok := a.spec.Cron[name]; ok {
			continue
		}
		a.stopJob(state)
		delete(a.cron, name)
	}
}

// syncHooks reconciles the hook set. Hooks have no schedule, so only their command and flags
// follow a rescan.
func (a *appRuntime) syncHooks() {
	if a.hooks == nil {
		a.hooks = map[string]*jobState{}
	}
	for name, hook := range a.spec.Hooks {
		state := a.hooks[name]
		if state == nil {
			a.hooks[name] = &jobState{kind: "hook", name: name, channel: hookName(name), command: hook.Command, timeout: hook.Timeout, overlap: hook.Overlap, disabled: hook.Disabled, restart: hook.Restart, pull: hook.Pull, runs: map[*jobRun]bool{}}
			continue
		}
		state.command, state.timeout, state.overlap, state.disabled, state.restart, state.pull = hook.Command, hook.Timeout, hook.Overlap, hook.Disabled, hook.Restart, hook.Pull
	}
	for name, state := range a.hooks {
		if _, ok := a.spec.Hooks[name]; ok {
			continue
		}
		a.stopJob(state)
		delete(a.hooks, name)
	}
}

// syncLifecycle reconciles the create and start steps the supervisor runs itself. destroy is
// run by the manager after the runtime is gone, so it never gets a job here.
func (a *appRuntime) syncLifecycle() {
	if a.lifecycle == nil {
		a.lifecycle = map[string]*jobState{}
	}
	for _, name := range []string{"create", "start"} {
		step, ok := a.spec.Lifecycle[name]
		state := a.lifecycle[name]
		if !ok {
			if state != nil {
				a.stopJob(state)
				delete(a.lifecycle, name)
			}
			continue
		}
		if state == nil {
			a.lifecycle[name] = &jobState{kind: "lifecycle", name: name, channel: stepName(name), command: step.Command, timeout: step.Timeout, runs: map[*jobRun]bool{}}
			continue
		}
		state.command, state.timeout = step.Command, step.Timeout
	}
}

// stopSteps kills a create or start step still running, so a stop never leaves one behind.
func (a *appRuntime) stopSteps() {
	for _, state := range a.lifecycle {
		if len(state.runs) > 0 {
			a.stopJob(state)
		}
	}
}

// cronTick fires every job whose next run has arrived, advancing the schedule whether or not the
// run is skipped for overlap.
func (a *appRuntime) cronTick(now time.Time) {
	for _, name := range slices.Sorted(maps.Keys(a.cron)) {
		state := a.cron[name]
		if state.disabled || state.next.IsZero() || now.Before(state.next) {
			continue
		}
		state.next = state.schedule.Next(now)
		if err := a.startJob(state, now, false); err != nil {
			logx.Warnf("%s/%s: %v", a.spec.Name, state.channel, err)
		}
	}
}

// runCron starts a job now at the operator's request, leaving its next scheduled run untouched.
func (a *appRuntime) runCron(name string, now time.Time) error {
	state := a.cron[name]
	if state == nil {
		return fmt.Errorf("unknown cron job %q", name)
	}
	if state.disabled {
		return fmt.Errorf("cron job %q is disabled", name)
	}
	return a.startJob(state, now, true)
}

// runHook starts one hook now, however it was triggered.
func (a *appRuntime) runHook(name string, now time.Time) error {
	state := a.hooks[name]
	if state == nil {
		return fmt.Errorf("unknown hook %q", name)
	}
	if state.disabled {
		return fmt.Errorf("hook %q is disabled", name)
	}
	return a.startJob(state, now, true)
}

func (a *appRuntime) startJob(state *jobState, now time.Time, manual bool) error {
	if len(state.runs) > 0 && !state.overlap {
		if manual {
			return fmt.Errorf("%s %q is still running", state.kind, state.name)
		}
		state.lastError = "skipped: previous run still going"
		return nil
	}
	env := processEnv(a.spec, state.name, 0, a.cfg.Socket, a.spec.Config.Env)
	if state.pull && a.spec.Config.GithubToken != "" {
		git.AuthEnv(env, a.spec.Config.GithubToken)
	}
	cmd, err := newCommand(a.spec.Dir, state.command, env)
	if err != nil {
		state.lastError = err.Error()
		return err
	}
	writer, err := a.jobLog(state)
	if err != nil {
		state.lastError = err.Error()
		return err
	}
	cmd.Stdout, cmd.Stderr = writer, writer
	_, _ = writer.Write(jobLine(state.kind, state.name, "start", 0, 0, nil))
	if err := cmd.Start(); err != nil {
		state.lastError = err.Error()
		_, _ = writer.Write(jobLine(state.kind, state.name, "error", -1, 0, err))
		return err
	}
	run := &jobRun{state: state, cmd: cmd, startedAt: time.Now(), waited: make(chan struct{}), log: writer}
	state.runs[run] = true
	state.lastStart = now
	state.lastError = ""
	go func() {
		err := cmd.Wait()
		run.finished.Store(true)
		close(run.waited)
		a.sendEvent(processEvent{kind: "job-exit", job: run, exitCode: exitCode(err), err: err})
	}()
	if state.timeout > 0 {
		run.timeout = time.AfterFunc(state.timeout, func() { a.sendEvent(processEvent{kind: "job-timeout", job: run}) })
	}
	return nil
}

func (a *appRuntime) jobExited(run *jobRun, exitCode int, err error) {
	state := run.state
	if state == nil || !state.runs[run] {
		return
	}
	if run.timeout != nil {
		run.timeout.Stop()
	}
	delete(state.runs, run)
	duration := time.Since(run.startedAt)
	state.lastEnd = time.Now()
	state.lastExit = exitCode
	if exitCode != 0 && state.lastError == "" {
		state.lastError = fmt.Sprintf("exit code %d", exitCode)
	}
	_, _ = run.log.Write(jobLine(state.kind, state.name, "exit", exitCode, duration, err))
	if state.kind == "lifecycle" {
		a.stepExited(state, exitCode)
		return
	}
	if state.kind == "hook" && exitCode != 0 {
		a.emit(notify.HookFailed, fmt.Sprintf("hook %s exited with code %d", state.name, exitCode))
	}
	// A deploy hook that finished cleanly brings the app onto the new release. The restart runs
	// through the manager so it drains first and never touches app state off the app goroutine.
	if state.kind == "hook" && state.restart && exitCode == 0 {
		a.emit(notify.Deploy, fmt.Sprintf("deploy hook %s succeeded", state.name))
		if a.restart != nil {
			name := a.spec.Name
			go func() { _ = a.restart(name) }()
		}
	}
}

func (a *appRuntime) jobTimeout(run *jobRun) {
	state := run.state
	if state == nil || !state.runs[run] || run.finished.Load() {
		return
	}
	state.lastError = fmt.Sprintf("timed out after %s", state.timeout)
	_, _ = run.log.Write(jobLine(state.kind, state.name, "timeout", 0, state.timeout, nil))
	_ = syscall.Kill(-run.cmd.Process.Pid, syscall.SIGKILL)
}

// stopJobs kills and reaps every running job and closes the job logs. It is called when the app
// is removed or the daemon shuts down; stopping the app itself leaves jobs alone.
func (a *appRuntime) stopJobs() {
	for name, state := range a.cron {
		a.stopJob(state)
		delete(a.cron, name)
	}
	for name, state := range a.hooks {
		a.stopJob(state)
		delete(a.hooks, name)
	}
	for name, state := range a.lifecycle {
		a.stopJob(state)
		delete(a.lifecycle, name)
	}
}

func (a *appRuntime) stopJob(state *jobState) {
	for run := range state.runs {
		if run.timeout != nil {
			run.timeout.Stop()
		}
		// Only signal while the process is unreaped; after Wait a reused pid must not be hit.
		if !run.finished.Load() {
			_ = syscall.Kill(-run.cmd.Process.Pid, syscall.SIGKILL)
		}
		<-run.waited
		delete(state.runs, run)
	}
	if state.log != nil {
		_ = state.log.Close()
		state.log = nil
	}
}

// jobLog opens the job's shared log file on first use.
func (a *appRuntime) jobLog(state *jobState) (*logWriter, error) {
	if state.log != nil {
		return state.log, nil
	}
	defaults := a.spec.Config.Process(state.channel)
	writer, err := newLogWriter(filepath.Join(a.cfg.LogDir, a.spec.Name, state.channel+".log"), int64(defaults.LogMaxSize), defaults.LogKeep)
	if err != nil {
		return nil, err
	}
	if a.echo != nil {
		writer.echo = a.echo.writer(a.spec.Name, state.channel)
	}
	state.log = writer
	return writer, nil
}

// sealJobLogs seals every cron, hook and lifecycle log so the ingestion module picks up finished runs.
func (a *appRuntime) sealJobLogs() error {
	for _, states := range []map[string]*jobState{a.cron, a.hooks, a.lifecycle} {
		for _, name := range slices.Sorted(maps.Keys(states)) {
			state := states[name]
			if state.log == nil {
				continue
			}
			if _, err := state.log.Seal(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *appRuntime) cronSnapshot() []CronSnapshot {
	names := slices.Sorted(maps.Keys(a.cron))
	result := make([]CronSnapshot, 0, len(names))
	for _, name := range names {
		state := a.cron[name]
		result = append(result, CronSnapshot{
			Name:      name,
			Schedule:  state.schedule.String(),
			Command:   state.command.Line,
			Disabled:  state.disabled,
			Next:      state.next,
			LastStart: state.lastStart,
			LastEnd:   state.lastEnd,
			LastExit:  state.lastExit,
			LastError: state.lastError,
			Running:   len(state.runs) > 0,
		})
	}
	return result
}

func (a *appRuntime) hookSnapshot() []HookSnapshot {
	names := slices.Sorted(maps.Keys(a.hooks))
	result := make([]HookSnapshot, 0, len(names))
	for _, name := range names {
		state := a.hooks[name]
		result = append(result, HookSnapshot{
			Name:      name,
			Command:   state.command.Line,
			Restart:   state.restart,
			Disabled:  state.disabled,
			LastStart: state.lastStart,
			LastEnd:   state.lastEnd,
			LastExit:  state.lastExit,
			LastError: state.lastError,
			Running:   len(state.runs) > 0,
		})
	}
	return result
}

// jobLine is the JSON marker written on start and exit so runs show up in the log store.
func jobLine(kind, job, event string, exitCode int, duration time.Duration, err error) []byte {
	payload := map[string]any{"level": "info", kind: job, "event": event}
	switch event {
	case "start":
		payload["message"] = fmt.Sprintf("%s %s started", kind, job)
	case "exit":
		if exitCode != 0 {
			payload["level"] = "error"
		}
		payload["exit_code"] = exitCode
		payload["duration_ms"] = duration.Milliseconds()
		payload["message"] = fmt.Sprintf("%s %s exited with code %d in %s", kind, job, exitCode, duration.Round(time.Millisecond))
	case "timeout":
		payload["level"] = "error"
		payload["message"] = fmt.Sprintf("%s %s timed out after %s", kind, job, duration)
	case "error":
		payload["level"] = "error"
		payload["message"] = fmt.Sprintf("%s %s could not start", kind, job)
	}
	if err != nil {
		payload["error"] = err.Error()
	}
	data, _ := json.Marshal(payload)
	return append(data, '\n')
}
