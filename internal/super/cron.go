package super

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"app-boss/internal/apps"
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

// cronState is one job's schedule and history. runs holds the one-shot processes currently going;
// with overlap false there is at most one.
type cronState struct {
	job       apps.CronJob
	next      time.Time
	runs      map[*cronRun]bool
	lastStart time.Time
	lastEnd   time.Time
	lastExit  int
	lastError string
	log       *logWriter
}

// cronRun is one execution of a job. It owns its command and wait channel so overlapping runs do
// not step on each other; the log writer is shared with the job.
type cronRun struct {
	name      string
	cmd       *exec.Cmd
	startedAt time.Time
	waited    chan struct{}
	log       *logWriter
}

// cronName is both the log channel and the file basename for a job.
func cronName(job string) string { return "cron-" + job }

// syncCron reconciles the runtime's schedule with the current app spec: new jobs are added, a
// changed schedule gets a new next run, removed jobs are killed and dropped.
func (a *appRuntime) syncCron(now time.Time) {
	if a.cron == nil {
		a.cron = map[string]*cronState{}
	}
	for name, job := range a.spec.Cron {
		state := a.cron[name]
		if state == nil {
			a.cron[name] = &cronState{job: job, next: job.Schedule.Next(now), runs: map[*cronRun]bool{}}
			continue
		}
		if state.job.Schedule.String() != job.Schedule.String() {
			state.next = job.Schedule.Next(now)
		}
		state.job = job
	}
	for name, state := range a.cron {
		if _, ok := a.spec.Cron[name]; ok {
			continue
		}
		a.stopCronState(name, state)
		delete(a.cron, name)
	}
}

// cronTick fires every job whose next run has arrived, advancing the schedule whether or not the
// run is skipped for overlap.
func (a *appRuntime) cronTick(now time.Time) {
	for _, name := range slices.Sorted(maps.Keys(a.cron)) {
		state := a.cron[name]
		if state.job.Disabled || state.next.IsZero() || now.Before(state.next) {
			continue
		}
		state.next = state.job.Schedule.Next(now)
		if err := a.startCronRun(name, now, false); err != nil {
			log.Printf("%s/%s: %v", a.spec.Name, cronName(name), err)
		}
	}
}

// runCron starts a job now at the operator's request, leaving its next scheduled run untouched.
func (a *appRuntime) runCron(name string, now time.Time) error {
	if a.cron[name] == nil {
		return fmt.Errorf("unknown cron job %q", name)
	}
	return a.startCronRun(name, now, true)
}

func (a *appRuntime) startCronRun(name string, now time.Time, manual bool) error {
	state := a.cron[name]
	if state == nil {
		return fmt.Errorf("unknown cron job %q", name)
	}
	if len(state.runs) > 0 && !state.job.Overlap {
		if manual {
			return fmt.Errorf("cron job %q is still running", name)
		}
		state.lastError = "skipped: previous run still going"
		return nil
	}
	command := state.job.Command
	var cmd *exec.Cmd
	if a.spec.Config.Shell {
		cmd = exec.Command("/bin/sh", "-c", command.Line)
	} else {
		resolved, err := resolveExecutable(command.Argv[0], a.spec.Dir, a.spec.Env["PATH"])
		if err != nil {
			state.lastError = err.Error()
			return err
		}
		argv := append([]string(nil), command.Argv...)
		argv[0] = resolved
		cmd = exec.Command(resolved, argv[1:]...)
	}
	writer, err := a.cronLog(state)
	if err != nil {
		state.lastError = err.Error()
		return err
	}
	cmd.Dir = a.spec.Dir
	cmd.Env = environment(a.spec, name, 0, a.cfg.Socket, nil)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = writer, writer
	_, _ = writer.Write(cronLine(name, "start", 0, 0, nil))
	if err := cmd.Start(); err != nil {
		state.lastError = err.Error()
		_, _ = writer.Write(cronLine(name, "error", -1, 0, err))
		return err
	}
	run := &cronRun{name: name, cmd: cmd, startedAt: time.Now(), waited: make(chan struct{}), log: writer}
	state.runs[run] = true
	state.lastStart = now
	state.lastError = ""
	go func() {
		err := cmd.Wait()
		close(run.waited)
		exitCode := 0
		if err != nil {
			exitCode = -1
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
		}
		select {
		case a.events <- processEvent{kind: "cron-exit", cron: run, exitCode: exitCode, err: err}:
		case <-a.ctx.Done():
		}
	}()
	if state.job.Timeout > 0 {
		time.AfterFunc(state.job.Timeout, func() {
			select {
			case a.events <- processEvent{kind: "cron-timeout", cron: run}:
			case <-a.ctx.Done():
			}
		})
	}
	return nil
}

func (a *appRuntime) cronRunExited(run *cronRun, exitCode int, err error) {
	state := a.cron[run.name]
	if state == nil || !state.runs[run] {
		return
	}
	delete(state.runs, run)
	duration := time.Since(run.startedAt)
	state.lastEnd = time.Now()
	state.lastExit = exitCode
	if exitCode != 0 && state.lastError == "" {
		state.lastError = fmt.Sprintf("exit code %d", exitCode)
	}
	_, _ = run.log.Write(cronLine(run.name, "exit", exitCode, duration, err))
}

func (a *appRuntime) cronRunTimeout(run *cronRun) {
	state := a.cron[run.name]
	if state == nil || !state.runs[run] {
		return
	}
	state.lastError = fmt.Sprintf("timed out after %s", state.job.Timeout)
	_, _ = run.log.Write(cronLine(run.name, "timeout", 0, state.job.Timeout, nil))
	_ = syscall.Kill(-run.cmd.Process.Pid, syscall.SIGKILL)
}

// stopCron kills and reaps every running job and closes the job log. It is called when the app is
// removed or the daemon shuts down; stopping the app itself leaves jobs alone.
func (a *appRuntime) stopCron() {
	for name, state := range a.cron {
		a.stopCronState(name, state)
		delete(a.cron, name)
	}
}

func (a *appRuntime) stopCronState(name string, state *cronState) {
	for run := range state.runs {
		_ = syscall.Kill(-run.cmd.Process.Pid, syscall.SIGKILL)
		<-run.waited
		delete(state.runs, run)
	}
	if state.log != nil {
		_ = state.log.Close()
		state.log = nil
	}
}

// cronLog opens the job's shared log file on first use.
func (a *appRuntime) cronLog(state *cronState) (*logWriter, error) {
	if state.log != nil {
		return state.log, nil
	}
	name := cronName(state.job.Command.Name)
	defaults := a.spec.Config.Process(name)
	writer, err := newLogWriter(filepath.Join(a.cfg.LogDir, a.spec.Name, name+".log"), int64(defaults.LogMaxSize), defaults.LogKeep)
	if err != nil {
		return nil, err
	}
	if a.echo != nil {
		writer.echo = a.echo.writer(a.spec.Name, name)
	}
	state.log = writer
	return writer, nil
}

// sealCronLogs seals the job logs so the ingestion module picks up finished runs.
func (a *appRuntime) sealCronLogs() ([]string, error) {
	var sealed []string
	for _, name := range slices.Sorted(maps.Keys(a.cron)) {
		state := a.cron[name]
		if state.log == nil {
			continue
		}
		path, err := state.log.Seal()
		if err != nil {
			return sealed, err
		}
		if path != "" {
			sealed = append(sealed, path)
		}
	}
	return sealed, nil
}

func (a *appRuntime) cronSnapshot() []CronSnapshot {
	names := slices.Sorted(maps.Keys(a.cron))
	result := make([]CronSnapshot, 0, len(names))
	for _, name := range names {
		state := a.cron[name]
		result = append(result, CronSnapshot{
			Name:      name,
			Schedule:  state.job.Schedule.String(),
			Command:   state.job.Command.Line,
			Disabled:  state.job.Disabled,
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

// cronLine is the JSON marker written on start and exit so runs show up in the log store.
func cronLine(job, event string, exitCode int, duration time.Duration, err error) []byte {
	payload := map[string]any{"level": "info", "cron": job, "event": event}
	switch event {
	case "start":
		payload["message"] = fmt.Sprintf("cron %s started", job)
	case "exit":
		if exitCode != 0 {
			payload["level"] = "error"
		}
		payload["exit_code"] = exitCode
		payload["duration_ms"] = duration.Milliseconds()
		payload["message"] = fmt.Sprintf("cron %s exited with code %d in %s", job, exitCode, duration.Round(time.Millisecond))
	case "timeout":
		payload["level"] = "error"
		payload["message"] = fmt.Sprintf("cron %s timed out after %s", job, duration)
	case "error":
		payload["level"] = "error"
		payload["message"] = fmt.Sprintf("cron %s could not start", job)
	}
	if err != nil {
		payload["error"] = err.Error()
	}
	data, _ := json.Marshal(payload)
	return append(data, '\n')
}
