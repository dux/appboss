package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"dboss/internal/apps"
	"dboss/internal/ctl"
	"dboss/internal/ops"
	"dboss/internal/supervisor"
)

// exec runs a one-off command. Flags are parsed only before the command starts, so the command's
// own flags (including -c) reach it untouched.
func (c CLI) exec(args []string) error {
	parsed, err := parseExecArgs(args)
	if err != nil {
		return err
	}
	here := &workdir{explicit: parsed.configPath}
	_, cfg, err := here.load()
	if err != nil {
		return err
	}
	app, command := "", parsed.rest
	if cfg.App != nil {
		app = filepath.Base(cfg.Dir)
	} else {
		if len(parsed.rest) < 2 {
			return errors.New("usage: dboss exec [app] <command> [args...]")
		}
		if _, lookupErr := apps.Lookup(cfg, parsed.rest[0]); lookupErr != nil {
			return fmt.Errorf("usage: dboss exec <app> <command> (%w)", lookupErr)
		}
		app, command = parsed.rest[0], parsed.rest[1:]
	}
	if len(command) == 0 {
		return errors.New("usage: dboss exec [app] <command> [args...]")
	}
	resolved, err := here.socket(parsed.socket)
	if err != nil {
		return err
	}
	client := ctl.Client{Socket: resolved}
	var result supervisor.ExecResult
	if err := client.Call(ctl.Request{Method: ops.ActionExec, App: app, Argv: command, Timeout: parsed.timeout}, &result); err != nil {
		return err
	}
	if parsed.json {
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
	} else {
		fmt.Fprint(c.Out, result.Output)
	}
	if result.ExitCode != 0 {
		return &exitError{code: result.ExitCode}
	}
	return nil
}

type execOptions struct {
	socket     string
	configPath string
	timeout    time.Duration
	json       bool
	rest       []string
}

// parseExecArgs reads the shared flags until the first positional, which begins the command.
func parseExecArgs(args []string) (execOptions, error) {
	var options execOptions
	for index := 0; index < len(args); {
		switch args[index] {
		case "--json":
			options.json = true
			index++
		case "--socket", "-c", "--config", "--timeout":
			if index+1 >= len(args) {
				return options, fmt.Errorf("%s requires a value", args[index])
			}
			value := args[index+1]
			switch args[index] {
			case "--socket":
				options.socket = value
			case "--timeout":
				parsed, err := time.ParseDuration(value)
				if err != nil {
					return options, fmt.Errorf("invalid --timeout %q", value)
				}
				options.timeout = parsed
			default:
				options.configPath = value
			}
			index += 2
		default:
			options.rest = args[index:]
			index = len(args)
		}
	}
	return options, nil
}
