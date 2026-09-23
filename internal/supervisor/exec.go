package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"dboss/internal/apps"
	"dboss/internal/logx"
	"dboss/internal/notify"
)

// runDestroyStep runs the app's destroy step once the app is stopped and detached, while its
// folder still exists. It is cleanup, so a failure is logged and notified but never blocks the
// destroy.
func (m *Manager) runDestroyStep(spec *apps.App) {
	step, ok := spec.Lifecycle["destroy"]
	if !ok {
		return
	}
	result, err := m.run(spec, stepName("destroy"), step.Command, step.Timeout)
	if err == nil && result.ExitCode != 0 {
		err = fmt.Errorf("exited with code %d", result.ExitCode)
	}
	if err != nil {
		logx.Warnf("%s: lifecycle destroy: %v: %s", spec.Name, err, lastBytes(result.Output, 400))
		m.emit(notify.Event{Type: notify.HookFailed, App: spec.Name, Error: "lifecycle destroy: " + err.Error(), Time: time.Now()})
		return
	}
	logx.Infof("%s: lifecycle destroy finished", spec.Name)
}

// lastBytes keeps the last n bytes of s, trimmed, for a one-line log of a command's output.
func lastBytes(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

// Exec runs one command in the app's environment and returns its combined output. It resolves
// the executable against the app PATH, captures both streams and kills the process group on
// timeout. It runs off the app's goroutine so a slow command cannot stall the supervisor.
func (m *Manager) Exec(name string, argv []string, timeout time.Duration) (ExecResult, error) {
	if len(argv) == 0 {
		return ExecResult{}, errors.New("no command given")
	}
	runtime, err := m.runtime(name)
	if err != nil {
		return ExecResult{}, err
	}
	response := runtime.query(request{kind: requestExecInfo})
	if response.err != nil {
		return ExecResult{}, response.err
	}
	return m.run(response.app, "exec", apps.Command{Argv: argv}, timeout)
}

// run is Exec for a spec already in hand: one command in the app folder with the app
// environment, combined output, and the process group killed on timeout.
func (m *Manager) run(spec *apps.App, procType string, line apps.Command, timeout time.Duration) (ExecResult, error) {
	env := processEnv(spec, procType, 0, m.cfg.Socket, spec.Config.Env)
	command, err := newCommand(spec.Dir, line, env)
	if err != nil {
		return ExecResult{}, err
	}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		return ExecResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(timeout):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		return ExecResult{Output: output.String(), ExitCode: -1}, fmt.Errorf("command timed out after %s", timeout)
	}
	return ExecResult{Output: output.String(), ExitCode: exitCode(waitErr)}, nil
}
