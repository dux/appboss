package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"dboss/internal/version"
)

type CLI struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

func (c CLI) Run(args []string) int {
	if c.In == nil {
		c.In = os.Stdin
	}
	if c.Out == nil {
		c.Out = os.Stdout
	}
	if c.Err == nil {
		c.Err = os.Stderr
	}
	if len(args) == 0 || args[0] == "help" && len(args) == 1 || wantsHelp(args[:1]) {
		c.usage(c.Out)
		return 0
	}
	command := args[0]
	if command == "help" {
		if err := c.help(c.Out, args[1]); err != nil {
			fmt.Fprintln(c.Err, "dboss:", err)
			return 2
		}
		return 0
	}
	cmd := findCommand(command)
	if cmd == nil {
		fmt.Fprintf(c.Err, "dboss: unknown command %q\n\n", command)
		c.usage(c.Err)
		return 2
	}
	command = cmd.name
	if wantsHelp(args[1:]) {
		_ = c.help(c.Out, command)
		return 0
	}
	var err error
	switch command {
	case "start":
		err = c.start(args[1:])
	case "systemd":
		err = c.systemd(args[1:])
	case "password":
		err = c.password(args[1:])
	case "init":
		err = c.init(args[1:])
	case "sshkey":
		err = c.sshkey(args[1:])
	case "trust":
		err = c.trust(args[1:])
	case "version":
		fmt.Fprintln(c.Out, version.String())
	case "update":
		err = c.update(args[1:])
	case "config":
		err = c.config(args[1:])
	case "check":
		err = c.check(args[1:])
	case "pages":
		err = c.pages(args[1:])
	case "doctor":
		err = c.doctor(args[1:])
	case "kill":
		err = c.kill(args[1:])
	case "deploy":
		err = c.deploy(args[1:])
	default:
		err = c.remote(command, args[1:])
	}
	if err != nil {
		var exitErr *exitError
		if errors.As(err, &exitErr) {
			return exitErr.code
		}
		fmt.Fprintln(c.Err, "dboss:", err)
		return 1
	}
	return 0
}

// exitError carries a child process's exit code out of `dboss exec` so it becomes dboss's own
// exit code, like a shell.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// configFlag registers -c and --config on set; both write to the same variable.
func configFlag(set *flag.FlagSet) *string {
	var path string
	set.StringVar(&path, "c", "", "config file (default: DBOSS_CONFIG, then ./dboss.local.yaml or ./dboss.yaml)")
	set.StringVar(&path, "config", "", "config file")
	return &path
}
