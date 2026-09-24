package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"reflect"
	"strings"

	"dboss/internal/ctl"
	"dboss/internal/logstore"
	"dboss/internal/ops"
	"dboss/internal/pg"
	"dboss/internal/pubsub"
	"dboss/internal/supervisor"
)

// results names the value each action answers with; an action missing here answers only ok.
var results = map[string]func() any{
	ops.ActionList:          func() any { return &[]supervisor.Snapshot{} },
	ops.ActionStatus:        func() any { return &supervisor.Snapshot{} },
	ops.ActionLogs:          func() any { return &map[string][]string{} },
	ops.ActionLogSearch:     func() any { return &[]logstore.LogEntry{} },
	ops.ActionPorts:         func() any { return &map[string]int{} },
	ops.ActionCron:          func() any { return &[]supervisor.CronSnapshot{} },
	ops.ActionHook:          func() any { return &[]supervisor.HookInfo{} },
	ops.ActionAudit:         func() any { return &[]logstore.AuditEntry{} },
	ops.ActionRescan:        func() any { return &ops.RescanResult{} },
	ctl.LoginMethod:         func() any { return &map[string]string{} },
	ops.ActionPG:            func() any { return &pg.Snapshot{} },
	ops.ActionPGBackups:     func() any { return &[]pg.Backup{} },
	ops.ActionPGBackup:      func() any { return &[]pg.Backup{} },
	ops.ActionPGRestore:     func() any { return &pg.RestoreResult{} },
	ops.ActionPGDrop:        func() any { return new(string) },
	ops.ActionPGDeleteDump:  func() any { return new(string) },
	ops.ActionPubsub:        func() any { return &[]pubsub.App{} },
	ops.ActionPubsubSecret:  func() any { return &ops.PubsubSecret{} },
	ops.ActionPubsubRotate:  func() any { return &ops.PubsubSecret{} },
	ops.ActionPubsubPublish: func() any { return &ops.PubsubPublished{} },
	ops.ActionAdd:           func() any { return &supervisor.Snapshot{} },
}

// call sends request and decodes the answer into the action's result type.
func call(client ctl.Client, request ctl.Request) (any, error) {
	newResult, ok := results[request.Method]
	if !ok {
		return map[string]any{"ok": true}, client.Call(request, nil)
	}
	target := newResult()
	if err := client.Call(request, target); err != nil {
		return nil, err
	}
	return reflect.ValueOf(target).Elem().Interface(), nil
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
	here := &workdir{explicit: opts.config}
	socket, err := here.socket(opts.socket)
	if err != nil {
		return err
	}
	client := ctl.Client{Socket: socket}
	request, err := c.parseRemote(command, opts, here)
	if err != nil || request.Method == "" {
		return err
	}
	if request.Method == ops.ActionLogs && opts.follow {
		return c.follow(client, request)
	}
	data, err := call(client, request)
	if err != nil {
		return err
	}
	if opts.json {
		encoded, _ := json.MarshalIndent(data, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	return c.printHuman(request.Method, data)
}

// parseRemote turns a command line into its control request. An empty Method means the command
// already answered on its own, like `pubsub help`.
func (c CLI) parseRemote(command string, opts *remoteOptions, here *workdir) (ctl.Request, error) {
	request := ctl.Request{Method: command}
	var err error
	switch command {
	case "ls", "rescan", "ports", ctl.LoginMethod:
		if len(opts.rest) != 0 {
			return request, fmt.Errorf("usage: dboss %s", command)
		}
	case "run", "stop", "restart", "destroy", "status":
		if command == "run" {
			request.Method = ops.ActionStart
		}
		if len(opts.rest) > 1 {
			return request, fmt.Errorf("usage: dboss %s [app]", command)
		}
		if request.App, err = here.app(opts.rest); err != nil {
			return request, fmt.Errorf("usage: dboss %s <app> (%w)", command, err)
		}
	case "maintenance":
		rest := opts.rest
		if len(rest) == 0 || len(rest) > 2 || (rest[len(rest)-1] != "on" && rest[len(rest)-1] != "off") {
			return request, errors.New("usage: dboss maintenance [app] on|off")
		}
		request.On = rest[len(rest)-1] == "on"
		if request.App, err = here.app(rest[:len(rest)-1]); err != nil {
			return request, fmt.Errorf("usage: dboss maintenance <app> on|off (%w)", err)
		}
	case "logs":
		return c.parseLogs(opts, here)
	case "cron":
		return parseJobs(opts.rest, here, "cron", ops.ActionCron, map[string]string{"run": ops.ActionCronRun})
	case "hooks":
		return parseJobs(opts.rest, here, "hooks", ops.ActionHook, map[string]string{"run": ops.ActionHookRun})
	case "audit":
		return c.parseAudit(opts.rest)
	case "pg":
		return c.parsePG(opts.rest)
	case "pubsub":
		return c.parsePubsub(opts.rest, here)
	case "add":
		return c.parseAdd(opts.rest)
	}
	return request, nil
}

func (c CLI) parseAdd(args []string) (ctl.Request, error) {
	request := ctl.Request{Method: ops.ActionAdd}
	set := flag.NewFlagSet("add", flag.ContinueOnError)
	set.SetOutput(c.Err)
	name := set.String("name", "", "app name")
	branch := set.String("branch", "", "branch to clone")
	host := set.String("host", "", "host that replaces the app's own")
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return request, err
	}
	if len(operands) != 1 {
		return request, errors.New("usage: dboss add <git-url> [--name name] [--branch branch] [--host host]")
	}
	request.Repo, request.App, request.Branch, request.Host = operands[0], *name, *branch, *host
	return request, nil
}

func (c CLI) parseLogs(opts *remoteOptions, here *workdir) (ctl.Request, error) {
	request := ctl.Request{Method: ops.ActionLogs}
	set := flag.NewFlagSet("logs", flag.ContinueOnError)
	set.SetOutput(c.Err)
	lines := set.Int("n", 200, "number of lines")
	follow := set.Bool("f", false, "follow")
	processName := set.String("process", "", "process name")
	search := set.String("search", "", "search the log store")
	level := set.String("level", "", "log level filter")
	channel := set.String("channel", "", "log channel filter")
	operands, err := parseSubcommandFlags(set, opts.rest)
	if err != nil {
		return request, err
	}
	if len(operands) > 1 {
		return request, errors.New("usage: dboss logs [app] [-f] [-n 200] [--process name] [--search q] [--level l] [--channel c]")
	}
	if request.App, err = here.app(operands); err != nil {
		return request, fmt.Errorf("usage: dboss logs <app> [-f] [-n 200] [--process name] (%w)", err)
	}
	request.Lines, request.Process = *lines, *processName
	if *follow && opts.json {
		return request, errors.New("--json and -f cannot be combined")
	}
	if *search != "" || *level != "" || *channel != "" {
		if *follow {
			return request, errors.New("-f tails the live files; drop it to search the store")
		}
		request.Method = ops.ActionLogSearch
		request.Query, request.Level, request.Channel = *search, *level, *channel
	}
	opts.follow = *follow
	return request, nil
}

// parseJobs reads `cron|hooks [app]` and `cron|hooks <sub> [app] <name>`: list is the method of
// the bare form, subs the method of each subcommand. The name lands in Job or Hook.
func parseJobs(args []string, here *workdir, command, list string, subs map[string]string) (ctl.Request, error) {
	request := ctl.Request{Method: list}
	if len(args) > 0 {
		if method, ok := subs[args[0]]; ok {
			sub, rest := args[0], args[1:]
			if len(rest) == 0 {
				return request, fmt.Errorf("usage: dboss %s %s [app] <name>", command, sub)
			}
			request.Method = method
			if command == "cron" {
				request.Job = rest[len(rest)-1]
			} else {
				request.Hook = rest[len(rest)-1]
			}
			var err error
			if request.App, err = here.app(rest[:len(rest)-1]); err != nil {
				return request, fmt.Errorf("usage: dboss %s %s <app> <name> (%w)", command, sub, err)
			}
			return request, nil
		}
	}
	var err error
	if request.App, err = here.app(args); err != nil {
		return request, fmt.Errorf("usage: dboss %s <app> (%w)", command, err)
	}
	return request, nil
}

func (c CLI) parseAudit(args []string) (ctl.Request, error) {
	request := ctl.Request{Method: ops.ActionAudit}
	set := flag.NewFlagSet("audit", flag.ContinueOnError)
	set.SetOutput(c.Err)
	app := set.String("app", "", "filter by app")
	actor := set.String("actor", "", "filter by actor")
	action := set.String("action", "", "filter by action")
	lines := set.Int("n", 200, "maximum rows")
	if err := set.Parse(args); err != nil {
		return request, err
	}
	request.App, request.ByActor, request.Action, request.Lines = *app, *actor, *action, *lines
	return request, nil
}

func (c CLI) parsePG(args []string) (ctl.Request, error) {
	var request ctl.Request
	switch {
	case len(args) == 0:
		request.Method = ops.ActionPG
	case args[0] == "backups":
		if len(args) != 1 {
			return request, errors.New("usage: dboss pg backups")
		}
		request.Method = ops.ActionPGBackups
	case args[0] == "backup":
		if len(args) > 2 {
			return request, errors.New("usage: dboss pg backup [database]")
		}
		request.Method = ops.ActionPGBackup
		if len(args) == 2 {
			request.Database = args[1]
		}
	case args[0] == "restore":
		set := flag.NewFlagSet("pg restore", flag.ContinueOnError)
		set.SetOutput(c.Err)
		target := set.String("target", "", "target database (default: <source>_restore)")
		force := set.Bool("force", false, "replace an existing target database")
		operands, err := parseSubcommandFlags(set, args[1:])
		if err != nil {
			return request, err
		}
		if len(operands) != 1 {
			return request, errors.New("usage: dboss pg restore <backup-id> [--target name] [--force]")
		}
		request.Method = ops.ActionPGRestore
		request.BackupID = operands[0]
		request.Target, request.Replace = *target, *force
		if *force {
			request.Confirm = *target
		}
	case args[0] == "delete":
		if len(args) != 2 {
			return request, errors.New("usage: dboss pg delete <backup-id>")
		}
		request.Method = ops.ActionPGDeleteDump
		request.BackupID = args[1]
	case args[0] == "drop":
		set := flag.NewFlagSet("pg drop", flag.ContinueOnError)
		set.SetOutput(c.Err)
		confirm := set.String("confirm", "", "repeat the database name to confirm")
		operands, err := parseSubcommandFlags(set, args[1:])
		if err != nil {
			return request, err
		}
		if len(operands) != 1 || *confirm == "" {
			return request, errors.New("usage: dboss pg drop <database> --confirm <database>")
		}
		request.Method = ops.ActionPGDrop
		request.Database, request.Confirm = operands[0], *confirm
	default:
		return request, errors.New("usage: dboss pg [backups | backup [database] | delete <backup-id> | restore <backup-id> | drop <database>]")
	}
	return request, nil
}

func (c CLI) parsePubsub(args []string, here *workdir) (ctl.Request, error) {
	request := ctl.Request{Method: ops.ActionPubsub}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "help":
		fmt.Fprintln(c.Out, pubsub.Help)
		return ctl.Request{}, nil
	case "rotate", "secret", "publish":
		request.Method = map[string]string{"rotate": ops.ActionPubsubRotate, "secret": ops.ActionPubsubSecret, "publish": ops.ActionPubsubPublish}[sub]
		set := flag.NewFlagSet("pubsub "+sub, flag.ContinueOnError)
		set.SetOutput(c.Err)
		process := set.String("process", "", "web process name when the app serves several hubs")
		var event, data *string
		if sub == "publish" {
			event = set.String("event", "message", "event name")
			data = set.String("data", "", `JSON payload, or - to read stdin`)
		}
		operands, err := parseSubcommandFlags(set, args[1:])
		if err != nil {
			return request, err
		}
		request.Process = *process
		if sub == "publish" {
			if len(operands) == 0 {
				return request, errors.New("usage: dboss pubsub publish [app] <channel> [--event name] [--data json|-] [--process name]")
			}
			request.Channel, operands = operands[len(operands)-1], operands[:len(operands)-1]
			request.Event = *event
			if request.Data, err = pubsubData(*data, c.In); err != nil {
				return request, err
			}
		}
		if request.App, err = here.app(operands); err != nil {
			return request, fmt.Errorf("usage: dboss pubsub %s [app] [--process name] (%w)", sub, err)
		}
	default:
		if len(args) > 0 {
			var err error
			if request.App, err = here.app(args); err != nil {
				return request, fmt.Errorf("usage: dboss pubsub [app] (%w)", err)
			}
		}
	}
	return request, nil
}

// parseSubcommandFlags parses a subcommand's flags wherever they appear among its operands. Go's
// flag package stops at the first operand, so `pg drop db --confirm db` would leave --confirm
// unset even though that is the order the help prints. Operands come back in the order given.
func parseSubcommandFlags(set *flag.FlagSet, args []string) ([]string, error) {
	var operands []string
	for {
		if err := set.Parse(args); err != nil {
			return nil, err
		}
		rest := set.Args()
		if len(rest) == 0 {
			return operands, nil
		}
		operands = append(operands, rest[0])
		args = rest[1:]
	}
}

// pubsubData turns the --data flag into a JSON payload. "-" reads stdin, valid JSON passes
// through, and anything else becomes a JSON string.
func pubsubData(value string, in io.Reader) (json.RawMessage, error) {
	if value == "-" {
		data, err := io.ReadAll(in)
		if err != nil {
			return nil, err
		}
		value = string(data)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return json.RawMessage("null"), nil
	}
	if json.Valid([]byte(value)) {
		return json.RawMessage(value), nil
	}
	return json.Marshal(value)
}
