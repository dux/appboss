package supervisor

import (
	"errors"
	"os/exec"
	"syscall"

	"dboss/internal/apps"
)

// newCommand builds one child of an app: a config line through /bin/sh -c, or the argv of
// `dboss exec` (already split by the caller's shell) with argv[0] resolved against the app's
// PATH. It runs in dir, in its own session, so a stop signals the shell and its children alike.
func newCommand(dir string, command apps.Command, env map[string]string) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	if command.Line != "" {
		cmd = exec.Command("/bin/sh", "-c", command.Line)
	} else {
		resolved, err := resolveExecutable(command.Argv[0], dir, env["PATH"])
		if err != nil {
			return nil, err
		}
		cmd = exec.Command(resolved, command.Argv[1:]...)
	}
	cmd.Dir = dir
	cmd.Env = envSlice(env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, nil
}

// exitCode is what Wait reported: 0 on success, the exit status, or -1 when the child did not
// exit on its own (killed by a signal, or never started).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
