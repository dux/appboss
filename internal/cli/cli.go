package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"app-boss/internal/apps"
	"app-boss/internal/config"
	"app-boss/internal/ctl"
	"app-boss/internal/daemon"
	"app-boss/internal/logstore"
	"app-boss/internal/ops"
	"app-boss/internal/super"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
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
			fmt.Fprintln(c.Err, "appboss:", err)
			return 2
		}
		return 0
	}
	if findCommand(command) == nil {
		fmt.Fprintf(c.Err, "appboss: unknown command %q\n\n", command)
		c.usage(c.Err)
		return 2
	}
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
	case "config", "check", "kill", "doctor":
		err = c.local(command, args[1:])
	default:
		err = c.remote(command, args[1:])
	}
	if err != nil {
		var exitErr *exitError
		if errors.As(err, &exitErr) {
			return exitErr.code
		}
		fmt.Fprintln(c.Err, "appboss:", err)
		return 1
	}
	return 0
}

// exitError carries a child process's exit code out of `appboss exec` so it becomes appboss's own
// exit code, like a shell.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// configFlag registers -c and --config on set; both write to the same variable.
func configFlag(set *flag.FlagSet) *string {
	var path string
	set.StringVar(&path, "c", "", "config file (default: APPBOSS_CONFIG, then ./appboss.local.yaml or ./appboss.yaml)")
	set.StringVar(&path, "config", "", "config file")
	return &path
}

// start runs the host session in the foreground. Under systemd this is the service process;
// on a terminal every app's output is echoed with an app/proc prefix.
func (c CLI) start(args []string) error {
	set := flag.NewFlagSet("start", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := configFlag(set)
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: appboss start [-c path]")
	}
	path, err := findConfig(*configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	var echo *super.Echo
	if info, statErr := os.Stdout.Stat(); statErr == nil && info.Mode()&os.ModeCharDevice != 0 {
		echo = super.NewEcho(c.Out)
	}
	session, err := daemon.Build(cfg, echo)
	if err != nil {
		return err
	}
	defer session.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return session.Run(ctx)
}

// password prints a bcrypt hash for basic_auth. The prompt hides input on a terminal; piped
// input is read as one line so the hash can be scripted.
func (c CLI) password(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: appboss password")
	}
	password, err := c.readPassword("Password: ")
	if err != nil {
		return err
	}
	if len(password) == 0 {
		return errors.New("password is empty")
	}
	if file, ok := c.In.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		confirm, err := c.readPassword("Confirm: ")
		if err != nil {
			return err
		}
		if string(confirm) != string(password) {
			return errors.New("passwords do not match")
		}
	}
	hash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.Out, string(hash))
	return nil
}

func (c CLI) readPassword(prompt string) ([]byte, error) {
	if file, ok := c.In.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fmt.Fprint(c.Err, prompt)
		defer fmt.Fprintln(c.Err)
		return term.ReadPassword(int(file.Fd()))
	}
	line, err := bufio.NewReader(c.In).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}

func (c CLI) local(command string, args []string) error {
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	set.SetOutput(c.Err)
	pathFlag := configFlag(set)
	jsonOutput := set.Bool("json", false, "JSON output")
	reference := set.Bool("reference", false, "print the annotated configuration reference")
	keys := set.Bool("keys", false, "list every configuration key with its description and default")
	var withDefaults bool
	set.BoolVar(&withDefaults, "d", false, "include defaults: print the resolved config")
	set.BoolVar(&withDefaults, "defaults", false, "include defaults: print the resolved config")
	if err := set.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if command == "config" && *reference {
		_, err := io.WriteString(c.Out, config.Reference)
		return err
	}
	if command == "config" && *keys {
		if set.NArg() > 1 {
			return errors.New("usage: appboss config --keys [filter]")
		}
		return c.printKeys(set.Arg(0), *jsonOutput)
	}
	path, err := findConfig(*pathFlag)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if command == "config" {
		positionals := set.Args()
		if len(positionals) > 0 && positionals[0] == "history" {
			return c.configHistory(cfg, positionals[1:], *jsonOutput)
		}
		if len(positionals) > 0 && positionals[0] == "restore" {
			return c.configRestore(cfg, positionals[1:], *jsonOutput)
		}
	}
	if command == "doctor" {
		if set.NArg() != 0 {
			return errors.New("usage: appboss doctor [-c path]")
		}
		return c.doctor(cfg, *jsonOutput)
	}
	if command == "kill" {
		if set.NArg() != 0 {
			return errors.New("usage: appboss kill [-c path]")
		}
		return c.kill(cfg, *jsonOutput)
	}
	if command == "check" {
		_, invalid, scanErr := apps.Discover(cfg)
		if scanErr != nil {
			return scanErr
		}
		if len(invalid) > 0 {
			for _, appErr := range invalid {
				fmt.Fprintln(c.Err, appErr)
			}
			return fmt.Errorf("%d invalid app(s)", len(invalid))
		}
		if *jsonOutput {
			fmt.Fprintln(c.Out, `{"ok":true,"invalid":[]}`)
		} else {
			fmt.Fprintln(c.Out, "ok")
		}
		return nil
	}
	if set.NArg() > 1 {
		return errors.New("usage: appboss config [app] [-d|--defaults]")
	}
	if set.NArg() == 0 {
		if withDefaults {
			return c.dump(cfg, *jsonOutput)
		}
		return c.dumpFile(cfg.SourcePath, *jsonOutput)
	}
	app, err := apps.Lookup(cfg, set.Arg(0))
	if err != nil {
		return err
	}
	if withDefaults {
		return c.dump(app.Config, *jsonOutput)
	}
	appPath, err := config.FindInDir(app.Dir)
	if err != nil {
		return err
	}
	return c.dumpFile(appPath, *jsonOutput)
}

// configHistory lists the saved revisions of a config file: the host file, or one app's file.
func (c CLI) configHistory(cfg config.Config, args []string, jsonOutput bool) error {
	id, err := configID(cfg, args)
	if err != nil {
		return err
	}
	revisions, err := apps.NewStore(cfg).History(id)
	if err != nil {
		return err
	}
	if jsonOutput {
		encoded, _ := json.MarshalIndent(revisions, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	if len(revisions) == 0 {
		fmt.Fprintln(c.Out, "no saved revisions")
		return nil
	}
	writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "REVISION\tSAVED\tSOURCE")
	for _, revision := range revisions {
		fmt.Fprintf(writer, "%s\t%s\t%s\n", revision.Revision, revision.Time.Format("2006-01-02 15:04:05"), revision.Source)
	}
	return writer.Flush()
}

// configRestore writes a saved revision back as the current file. The running host picks it up on
// the next rescan.
func (c CLI) configRestore(cfg config.Config, args []string, jsonOutput bool) error {
	if len(args) == 0 {
		return errors.New("usage: appboss config restore [app] <revision>")
	}
	revision := args[len(args)-1]
	id, err := configID(cfg, args[:len(args)-1])
	if err != nil {
		return err
	}
	file, err := apps.NewStore(cfg).Restore(id, revision)
	if err != nil {
		return err
	}
	if jsonOutput {
		encoded, _ := json.MarshalIndent(file, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	fmt.Fprintf(c.Out, "restored %s to revision %s; run `appboss rescan` to apply it\n", file.ID, revision)
	return nil
}

// configID resolves the history/cache id for the optional app argument: the app's file, or the
// host file inside a host folder.
func configID(cfg config.Config, args []string) (string, error) {
	if len(args) == 0 {
		if cfg.App != nil {
			return "app:" + filepath.Base(cfg.Dir), nil
		}
		return "host", nil
	}
	if len(args) != 1 {
		return "", errors.New("usage: appboss config history|restore [app]")
	}
	app, err := apps.Lookup(cfg, args[0])
	if err != nil {
		return "", err
	}
	return "app:" + app.Name, nil
}

// doctor runs the preflight checks a first start or a deploy needs: the tools, the writable
// directories, a valid config and any listeners still holding the app port range.
func (c CLI) doctor(cfg config.Config, jsonOutput bool) error {
	type finding struct {
		Level   string `json:"level"`
		Message string `json:"message"`
	}
	var findings []finding
	failed := false
	add := func(level, message string) {
		findings = append(findings, finding{Level: level, Message: message})
		if level == "fail" {
			failed = true
		}
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		add("fail", "lsof is not on PATH; appboss needs it to clear the port range")
	} else {
		add("ok", "lsof found")
	}
	for _, dir := range []struct{ name, path string }{{"state_dir", cfg.StateDir}, {"log_dir", cfg.LogDir}, {"socket dir", filepath.Dir(cfg.Socket)}} {
		if err := writable(dir.path); err != nil {
			add("fail", fmt.Sprintf("%s %s is not writable: %v", dir.name, dir.path, err))
			continue
		}
		add("ok", fmt.Sprintf("%s %s is writable", dir.name, dir.path))
	}
	_, invalid, scanErr := apps.Discover(cfg)
	switch {
	case scanErr != nil:
		add("fail", "config: "+scanErr.Error())
	case len(invalid) > 0:
		for _, appErr := range invalid {
			add("fail", appErr.Error())
		}
	default:
		add("ok", "config and every app are valid")
	}
	if pids, err := super.ListenersInRange(cfg.Ports.Range); err != nil {
		add("warn", "port range check failed: "+err.Error())
	} else if len(pids) > 0 {
		add("warn", fmt.Sprintf("port range %d-%d has listeners (pids %v); a start clears them", cfg.Ports.Range[0], cfg.Ports.Range[1], pids))
	} else {
		add("ok", fmt.Sprintf("port range %d-%d is clear", cfg.Ports.Range[0], cfg.Ports.Range[1]))
	}
	if jsonOutput {
		encoded, _ := json.MarshalIndent(map[string]any{"ok": !failed, "findings": findings}, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
	} else {
		for _, item := range findings {
			fmt.Fprintf(c.Out, "%-4s %s\n", item.Level, item.Message)
		}
	}
	if failed {
		return errors.New("doctor found problems")
	}
	return nil
}

// writable checks that a directory can be created and a file written inside it.
func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".appboss-doctor-*")
	if err != nil {
		return err
	}
	name := file.Name()
	_ = file.Close()
	return os.Remove(name)
}

// dumpFile prints a config file as written, comments included; it has already been validated.
func (c CLI) dumpFile(path string, jsonOutput bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !jsonOutput {
		_, err = c.Out.Write(data)
		return err
	}
	var given map[string]any
	if err := yaml.Unmarshal(data, &given); err != nil {
		return err
	}
	return c.dump(given, true)
}

// dump prints a resolved value as 2-space YAML or indented JSON.
func (c CLI) dump(value any, jsonOutput bool) error {
	if jsonOutput {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		_, err = c.Out.Write(append(data, '\n'))
		return err
	}
	encoder := yaml.NewEncoder(c.Out)
	encoder.SetIndent(2)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	return encoder.Close()
}

func (c CLI) kill(cfg config.Config, jsonOutput bool) error {
	client := ctl.Client{Socket: cfg.Socket}
	var snapshots []super.Snapshot
	err := client.Call(ctl.Request{Method: "ls"}, &snapshots)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("connect to daemon: %w", err)
	}
	stopped := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if err := client.Call(ctl.Request{Method: "stop", App: snapshot.Name}, nil); err != nil {
			return fmt.Errorf("stop %s: %w", snapshot.Name, err)
		}
		stopped = append(stopped, snapshot.Name)
	}
	pids, err := super.ClearPortRange(cfg.Ports.Range, cfg.Defaults.StopTimeout.Value())
	if err != nil {
		return err
	}
	if jsonOutput {
		encoded, _ := json.Marshal(map[string]any{"stopped": stopped, "killed_pids": pids})
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	fmt.Fprintf(c.Out, "stopped %d app(s); killed %d remaining listener(s) in ports %d-%d\n", len(stopped), len(pids), cfg.Ports.Range[0], cfg.Ports.Range[1])
	return nil
}

// remote sends one command to the running host over its control socket. Commands that take an
// app default to the current folder's app when run inside one.
func (c CLI) remote(command string, args []string) error {
	if command == "exec" {
		return c.exec(args)
	}
	opts, err := commonArgs(args)
	if err != nil {
		return err
	}
	socket, err := findSocket(opts.socket, opts.config)
	if err != nil {
		return err
	}
	client := ctl.Client{Socket: socket}
	request := ctl.Request{Method: command}
	if command == "run" {
		request.Method = "start"
	}
	switch command {
	case "ls", "rescan", "ports", "login":
		if len(opts.rest) != 0 {
			return fmt.Errorf("usage: appboss %s", command)
		}
	case "run", "stop", "restart", "status":
		if len(opts.rest) > 1 {
			return fmt.Errorf("usage: appboss %s [app]", command)
		}
		if request.App, err = appArgument(opts.rest, opts.config); err != nil {
			return fmt.Errorf("usage: appboss %s <app> (%w)", command, err)
		}
	case "maintenance":
		if len(opts.rest) == 0 || len(opts.rest) > 2 || (opts.rest[len(opts.rest)-1] != "on" && opts.rest[len(opts.rest)-1] != "off") {
			return errors.New("usage: appboss maintenance [app] on|off")
		}
		request.On = opts.rest[len(opts.rest)-1] == "on"
		if request.App, err = appArgument(opts.rest[:len(opts.rest)-1], opts.config); err != nil {
			return fmt.Errorf("usage: appboss maintenance <app> on|off (%w)", err)
		}
	case "logs":
		var appArgs []string
		if len(opts.rest) > 0 && !strings.HasPrefix(opts.rest[0], "-") {
			appArgs, opts.rest = opts.rest[:1], opts.rest[1:]
		}
		if request.App, err = appArgument(appArgs, opts.config); err != nil {
			return fmt.Errorf("usage: appboss logs <app> [-f] [-n 200] [--process name] (%w)", err)
		}
		set := flag.NewFlagSet("logs", flag.ContinueOnError)
		set.SetOutput(c.Err)
		lines := set.Int("n", 200, "number of lines")
		follow := set.Bool("f", false, "follow")
		processName := set.String("process", "", "process name")
		search := set.String("search", "", "search the log store")
		level := set.String("level", "", "log level filter")
		channel := set.String("channel", "", "log channel filter")
		if err := set.Parse(opts.rest); err != nil {
			return err
		}
		if set.NArg() != 0 {
			return errors.New("usage: appboss logs [app] [-f] [-n 200] [--process name] [--search q] [--level l] [--channel c]")
		}
		request.Lines, request.Process = *lines, *processName
		if *follow && opts.json {
			return errors.New("--json and -f cannot be combined")
		}
		if *search != "" || *level != "" || *channel != "" {
			if *follow {
				return errors.New("-f tails the live files; drop it to search the store")
			}
			request.Method = ops.ActionLogSearch
			request.Query, request.Level, request.Channel = *search, *level, *channel
		} else if *follow {
			return c.follow(client, request)
		}
	case "cron":
		if len(opts.rest) > 0 && opts.rest[0] == "run" {
			request.Method = ops.ActionCronRun
			jobs := opts.rest[1:]
			if len(jobs) == 0 {
				return errors.New("usage: appboss cron run [app] <job>")
			}
			request.Job = jobs[len(jobs)-1]
			if request.App, err = appArgument(jobs[:len(jobs)-1], opts.config); err != nil {
				return fmt.Errorf("usage: appboss cron run <app> <job> (%w)", err)
			}
		} else {
			request.Method = ops.ActionCron
			if request.App, err = appArgument(opts.rest, opts.config); err != nil {
				return fmt.Errorf("usage: appboss cron <app> (%w)", err)
			}
		}
	case "hooks":
		hooks := opts.rest
		if len(hooks) > 0 && (hooks[0] == "run" || hooks[0] == "rotate") {
			sub, args := hooks[0], hooks[1:]
			if len(args) == 0 {
				return fmt.Errorf("usage: appboss hooks %s [app] <hook>", sub)
			}
			request.Hook = args[len(args)-1]
			if sub == "run" {
				request.Method = ops.ActionHookRun
			} else {
				request.Method = ops.ActionHookRotate
			}
			if request.App, err = appArgument(args[:len(args)-1], opts.config); err != nil {
				return fmt.Errorf("usage: appboss hooks %s <app> <hook> (%w)", sub, err)
			}
		} else {
			request.Method = ops.ActionHook
			if request.App, err = appArgument(hooks, opts.config); err != nil {
				return fmt.Errorf("usage: appboss hooks <app> (%w)", err)
			}
		}
	case "audit":
		request.Method = ops.ActionAudit
		set := flag.NewFlagSet("audit", flag.ContinueOnError)
		set.SetOutput(c.Err)
		app := set.String("app", "", "filter by app")
		actor := set.String("actor", "", "filter by actor")
		action := set.String("action", "", "filter by action")
		lines := set.Int("n", 200, "maximum rows")
		if err := set.Parse(opts.rest); err != nil {
			return err
		}
		request.App, request.Actor, request.Action, request.Lines = *app, *actor, *action, *lines
	}
	jsonOutput := opts.json
	var data any
	switch request.Method {
	case "ls":
		var snapshots []super.Snapshot
		if err := client.Call(request, &snapshots); err != nil {
			return err
		}
		data = snapshots
	case "status":
		var snapshot super.Snapshot
		if err := client.Call(request, &snapshot); err != nil {
			return err
		}
		data = snapshot
	case "logs":
		var logs map[string][]string
		if err := client.Call(request, &logs); err != nil {
			return err
		}
		data = logs
	case "log-search":
		var rows []logstore.LogEntry
		if err := client.Call(request, &rows); err != nil {
			return err
		}
		data = rows
	case "ports":
		var entries map[string]int
		if err := client.Call(request, &entries); err != nil {
			return err
		}
		data = entries
	case "cron":
		var jobs []super.CronSnapshot
		if err := client.Call(request, &jobs); err != nil {
			return err
		}
		data = jobs
	case "hook":
		var hooks []super.HookInfo
		if err := client.Call(request, &hooks); err != nil {
			return err
		}
		data = hooks
	case "hook-rotate":
		var result map[string]any
		if err := client.Call(request, &result); err != nil {
			return err
		}
		data = result
	case "audit":
		var rows []logstore.AuditEntry
		if err := client.Call(request, &rows); err != nil {
			return err
		}
		data = rows
	case "rescan":
		var result map[string]any
		if err := client.Call(request, &result); err != nil {
			return err
		}
		data = result
	case "login":
		var result map[string]string
		if err := client.Call(request, &result); err != nil {
			return err
		}
		data = result
	default:
		if err := client.Call(request, nil); err != nil {
			return err
		}
		data = map[string]any{"ok": true}
	}
	if jsonOutput {
		encoded, _ := json.MarshalIndent(data, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	return c.printHuman(request.Method, data)
}

// exec runs a one-off command. Flags are parsed only before the command starts, so the command's
// own flags (including -c) reach it untouched.
func (c CLI) exec(args []string) error {
	parsed, err := parseExecArgs(args)
	if err != nil {
		return err
	}
	path, err := findConfig(parsed.configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	app, command := "", parsed.rest
	if cfg.App != nil {
		app = filepath.Base(cfg.Dir)
	} else {
		if len(parsed.rest) < 2 {
			return errors.New("usage: appboss exec [app] <command> [args...]")
		}
		if _, lookupErr := apps.Lookup(cfg, parsed.rest[0]); lookupErr != nil {
			return fmt.Errorf("usage: appboss exec <app> <command> (%w)", lookupErr)
		}
		app, command = parsed.rest[0], parsed.rest[1:]
	}
	if len(command) == 0 {
		return errors.New("usage: appboss exec [app] <command> [args...]")
	}
	resolved, err := findSocket(parsed.socket, parsed.configPath)
	if err != nil {
		return err
	}
	client := ctl.Client{Socket: resolved}
	var result super.ExecResult
	if err := client.Call(ctl.Request{Method: ops.ActionExec, App: app, Argv: command, Timeout: parsed.timeout}, &result); err != nil {
		return err
	}
	if parsed.json {
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	fmt.Fprint(c.Out, result.Output)
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

// flagsFirst moves flags ahead of positional arguments so `appboss config app -d` and
// `appboss config --keys static --json` parse the same as with the flags in front.
func flagsFirst(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		if (arg == "-c" || arg == "--config") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

var keyGroups = []struct{ id, title, note string }{
	{config.GroupHost, "Host keys", "appboss.yaml with apps:"},
	{config.GroupApp, "App keys", "appboss.yaml with procfile:"},
	{config.GroupShared, "Shared app keys", "defaults: in the host file, top level in an app file; per-process ones also under processes.<name>"},
}

// printKeys lists the documented config keys, grouped by file role. filter narrows by a
// substring of the key path.
func (c CLI) printKeys(filter string, jsonOutput bool) error {
	var keys []config.Key
	for _, key := range config.Keys() {
		if filter == "" || strings.Contains(key.Path, filter) {
			keys = append(keys, key)
		}
	}
	if jsonOutput {
		encoded, _ := json.MarshalIndent(keys, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	if len(keys) == 0 {
		return fmt.Errorf("no config key matches %q", filter)
	}
	writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	for _, group := range keyGroups {
		first := true
		for _, key := range keys {
			if key.Group != group.id {
				continue
			}
			if first {
				fmt.Fprintf(writer, "%s  (%s)\n", group.title, group.note)
				first = false
			}
			value := key.Default
			if value == "" {
				value = "e.g. " + key.Example
			}
			// The last text on a line is not a padded cell, so lines without the scope
			// column end right after the value instead of trailing spaces.
			if key.PerProcess {
				fmt.Fprintf(writer, "  %s\t%s\t%s\tper process\n", key.Path, key.Description, value)
			} else {
				fmt.Fprintf(writer, "  %s\t%s\t%s\n", key.Path, key.Description, value)
			}
		}
		if !first {
			fmt.Fprintln(writer)
		}
	}
	return writer.Flush()
}

func (c CLI) printHuman(method string, data any) error {
	switch method {
	case "ls":
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "APP\tSTATE\tPORTS\tUPTIME\tLAST ACTIVITY\tMEM (APPROX)")
		for _, snapshot := range data.([]super.Snapshot) {
			var values []string
			for _, process := range snapshot.Processes {
				if process.State != super.Running {
					continue
				}
				values = append(values, fmt.Sprintf("%s:%d", process.Name, process.Port))
			}
			last := "-"
			if !snapshot.LastActivity.IsZero() {
				last = snapshot.LastActivity.Format(time.RFC3339)
			}
			state := string(snapshot.State)
			if snapshot.Draining {
				state += " draining"
			}
			if snapshot.Maintenance {
				state += " maintenance"
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%d\n", snapshot.Name, state, strings.Join(values, ","), snapshot.Uptime, last, snapshot.Resources.MemoryBytes)
		}
		return writer.Flush()
	case "status":
		encoded, _ := json.MarshalIndent(data, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
	case "logs":
		logs := data.(map[string][]string)
		names := slices.Sorted(maps.Keys(logs))
		for _, name := range names {
			for _, line := range logs[name] {
				fmt.Fprintf(c.Out, "[%s] %s\n", name, line)
			}
		}
	case "log-search":
		rows := data.([]logstore.LogEntry)
		if len(rows) == 0 {
			fmt.Fprintln(c.Out, "no matching log rows")
			return nil
		}
		for _, row := range rows {
			fmt.Fprintf(c.Out, "%s %-5s %-12s %s\n", row.Time.Local().Format("2006-01-02 15:04:05"), strings.ToUpper(row.Level), row.Process, row.Message)
		}
	case "ports":
		entries := data.(map[string]int)
		names := slices.Sorted(maps.Keys(entries))
		for _, name := range names {
			fmt.Fprintf(c.Out, "%s\t%d\n", name, entries[name])
		}
	case "cron":
		jobs := data.([]super.CronSnapshot)
		if len(jobs) == 0 {
			fmt.Fprintln(c.Out, "no scheduled jobs")
			return nil
		}
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "JOB\tSCHEDULE\tNEXT\tLAST\tCOMMAND")
		for _, job := range jobs {
			next := "-"
			if !job.Next.IsZero() {
				next = job.Next.Format("2006-01-02 15:04")
			}
			last := "-"
			switch {
			case job.Running:
				last = "running"
			case job.LastError != "":
				last = job.LastError
			case !job.LastEnd.IsZero():
				last = fmt.Sprintf("exit %d at %s", job.LastExit, job.LastEnd.Format("15:04"))
			}
			name := job.Name
			if job.Disabled {
				name += " (disabled)"
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", name, job.Schedule, next, last, job.Command)
		}
		return writer.Flush()
	case "hook":
		hooks := data.([]super.HookInfo)
		if len(hooks) == 0 {
			fmt.Fprintln(c.Out, "no hooks")
			return nil
		}
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "HOOK\tSOURCE\tLAST\tCOMMAND\tURL")
		for _, hook := range hooks {
			last := "-"
			switch {
			case hook.Running:
				last = "running"
			case hook.LastError != "":
				last = hook.LastError
			case !hook.LastEnd.IsZero():
				last = fmt.Sprintf("exit %d at %s", hook.LastExit, hook.LastEnd.Format("15:04"))
			}
			name := hook.Name
			if hook.Disabled {
				name += " (disabled)"
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", name, hook.Source, last, hook.Command, hook.URL)
		}
		return writer.Flush()
	case "hook-rotate":
		info := data.(map[string]any)["hook"].(map[string]any)
		fmt.Fprintln(c.Out, "rotated; new ping URL:")
		fmt.Fprintln(c.Out, info["url"])
	case "audit":
		rows := data.([]logstore.AuditEntry)
		if len(rows) == 0 {
			fmt.Fprintln(c.Out, "no audit rows")
			return nil
		}
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "TIME\tACTOR\tACTION\tAPP\tDETAIL\tRESULT")
		for _, row := range rows {
			result := row.Result
			if row.Error != "" {
				result += ": " + row.Error
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", row.Time.Local().Format("2006-01-02 15:04:05"), row.Actor, row.Action, row.App, row.Detail, result)
		}
		return writer.Flush()
	case "rescan":
		result := data.(map[string]any)
		invalid, _ := result["invalid"].([]any)
		fmt.Fprintf(c.Out, "rescan complete (%d invalid)\n", len(invalid))
		for _, message := range invalid {
			fmt.Fprintf(c.Out, "  %v\n", message)
		}
		if keys, _ := result["restart_required"].([]any); len(keys) > 0 {
			fmt.Fprintf(c.Out, "restart required: %s changed (systemctl restart appboss, or Ctrl-C and appboss start)\n", joinAny(keys))
		}
	case "login":
		fmt.Fprintln(c.Out, data.(map[string]string)["url"])
		fmt.Fprintln(c.Out, "Opens the console as cli@localhost. Valid for 3 minutes, one use.")
	default:
		fmt.Fprintln(c.Out, "ok")
	}
	return nil
}

func joinAny(values []any) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = fmt.Sprint(value)
	}
	return strings.Join(parts, ", ")
}

func (c CLI) follow(client ctl.Client, request ctl.Request) error {
	previous := map[string][]string{}
	for {
		var logs map[string][]string
		if err := client.Call(request, &logs); err != nil {
			return err
		}
		names := slices.Sorted(maps.Keys(logs))
		for _, name := range names {
			start := logOverlap(previous[name], logs[name])
			for _, line := range logs[name][start:] {
				fmt.Fprintf(c.Out, "[%s] %s\n", name, line)
			}
			previous[name] = append(previous[name][:0], logs[name]...)
		}
		time.Sleep(time.Second)
	}
}

func logOverlap(previous, current []string) int {
	maximum := min(len(previous), len(current))
	for count := maximum; count > 0; count-- {
		match := true
		for index := 0; index < count; index++ {
			if previous[len(previous)-count+index] != current[index] {
				match = false
				break
			}
		}
		if match {
			return count
		}
	}
	return 0
}

type remoteOptions struct {
	json   bool
	socket string
	config string
	rest   []string
}

// commonArgs pulls the flags shared by every remote command out of args, wherever they appear.
func commonArgs(args []string) (remoteOptions, error) {
	var opts remoteOptions
	opts.rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			opts.json = true
		case "--socket", "-c", "--config":
			if i+1 >= len(args) {
				return remoteOptions{}, fmt.Errorf("%s requires a path", args[i])
			}
			if args[i] == "--socket" {
				opts.socket = args[i+1]
			} else {
				opts.config = args[i+1]
			}
			i++
		default:
			opts.rest = append(opts.rest, args[i])
		}
	}
	return opts, nil
}

const defaultSocket = "/run/appboss/appboss.sock"

// findSocket resolves the control socket: --socket, APPBOSS_SOCKET, the socket of the config in
// reach if it exists on disk, then the well-known production path. The last step is what lets
// `appboss restart` inside a deployed app folder reach the host session started elsewhere.
func findSocket(explicit, configPath string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("APPBOSS_SOCKET"); env != "" {
		return env, nil
	}
	path, err := findConfig(configPath)
	if err != nil {
		return defaultSocket, nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(cfg.Socket); err == nil {
		return cfg.Socket, nil
	}
	return defaultSocket, nil
}

// appArgument returns the explicit app name or, inside an app folder, that folder's app.
func appArgument(args []string, configPath string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	path, err := findConfig(configPath)
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", err
	}
	if cfg.App == nil {
		return "", fmt.Errorf("%s is a host config, name the app", path)
	}
	return filepath.Base(cfg.Dir), nil
}

func findConfig(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("APPBOSS_CONFIG"); env != "" {
		return env, nil
	}
	path, err := config.FindInDir(".")
	if err != nil {
		return "", fmt.Errorf("%w (use -c or APPBOSS_CONFIG)", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return absolute, nil
}
