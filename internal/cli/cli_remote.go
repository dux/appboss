package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"

	"dboss/internal/ctl"
	"dboss/internal/logstore"
	"dboss/internal/ops"
	"dboss/internal/pg"
	"dboss/internal/pubsub"
	"dboss/internal/super"
)

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
			return fmt.Errorf("usage: dboss %s", command)
		}
	case "run", "stop", "restart", "destroy", "status":
		if len(opts.rest) > 1 {
			return fmt.Errorf("usage: dboss %s [app]", command)
		}
		if request.App, err = appArgument(opts.rest, opts.config); err != nil {
			return fmt.Errorf("usage: dboss %s <app> (%w)", command, err)
		}
	case "maintenance":
		if len(opts.rest) == 0 || len(opts.rest) > 2 || (opts.rest[len(opts.rest)-1] != "on" && opts.rest[len(opts.rest)-1] != "off") {
			return errors.New("usage: dboss maintenance [app] on|off")
		}
		request.On = opts.rest[len(opts.rest)-1] == "on"
		if request.App, err = appArgument(opts.rest[:len(opts.rest)-1], opts.config); err != nil {
			return fmt.Errorf("usage: dboss maintenance <app> on|off (%w)", err)
		}
	case "logs":
		var appArgs []string
		if len(opts.rest) > 0 && !strings.HasPrefix(opts.rest[0], "-") {
			appArgs, opts.rest = opts.rest[:1], opts.rest[1:]
		}
		if request.App, err = appArgument(appArgs, opts.config); err != nil {
			return fmt.Errorf("usage: dboss logs <app> [-f] [-n 200] [--process name] (%w)", err)
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
			return errors.New("usage: dboss logs [app] [-f] [-n 200] [--process name] [--search q] [--level l] [--channel c]")
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
				return errors.New("usage: dboss cron run [app] <job>")
			}
			request.Job = jobs[len(jobs)-1]
			if request.App, err = appArgument(jobs[:len(jobs)-1], opts.config); err != nil {
				return fmt.Errorf("usage: dboss cron run <app> <job> (%w)", err)
			}
		} else {
			request.Method = ops.ActionCron
			if request.App, err = appArgument(opts.rest, opts.config); err != nil {
				return fmt.Errorf("usage: dboss cron <app> (%w)", err)
			}
		}
	case "hooks":
		hooks := opts.rest
		if len(hooks) > 0 && (hooks[0] == "run" || hooks[0] == "rotate") {
			sub, args := hooks[0], hooks[1:]
			if len(args) == 0 {
				return fmt.Errorf("usage: dboss hooks %s [app] <hook>", sub)
			}
			request.Hook = args[len(args)-1]
			if sub == "run" {
				request.Method = ops.ActionHookRun
			} else {
				request.Method = ops.ActionHookRotate
			}
			if request.App, err = appArgument(args[:len(args)-1], opts.config); err != nil {
				return fmt.Errorf("usage: dboss hooks %s <app> <hook> (%w)", sub, err)
			}
		} else {
			request.Method = ops.ActionHook
			if request.App, err = appArgument(hooks, opts.config); err != nil {
				return fmt.Errorf("usage: dboss hooks <app> (%w)", err)
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
	case "pg":
		pgArgs := opts.rest
		switch {
		case len(pgArgs) == 0:
			request.Method = ops.ActionPG
		case pgArgs[0] == "backups":
			if len(pgArgs) != 1 {
				return errors.New("usage: dboss pg backups")
			}
			request.Method = ops.ActionPGBackups
		case pgArgs[0] == "backup":
			if len(pgArgs) > 2 {
				return errors.New("usage: dboss pg backup [database]")
			}
			request.Method = ops.ActionPGBackup
			if len(pgArgs) == 2 {
				request.Database = pgArgs[1]
			}
		case pgArgs[0] == "restore":
			set := flag.NewFlagSet("pg restore", flag.ContinueOnError)
			set.SetOutput(c.Err)
			target := set.String("target", "", "target database (default: <source>_restore)")
			force := set.Bool("force", false, "replace an existing target database")
			if err := set.Parse(pgArgs[1:]); err != nil {
				return err
			}
			if set.NArg() != 1 {
				return errors.New("usage: dboss pg restore <backup-id> [--target name] [--force]")
			}
			request.Method = ops.ActionPGRestore
			request.BackupID = set.Arg(0)
			request.Target, request.Replace = *target, *force
			if *force {
				request.Confirm = *target
			}
		case pgArgs[0] == "delete":
			if len(pgArgs) != 2 {
				return errors.New("usage: dboss pg delete <backup-id>")
			}
			request.Method = ops.ActionPGDeleteDump
			request.BackupID = pgArgs[1]
		case pgArgs[0] == "drop":
			set := flag.NewFlagSet("pg drop", flag.ContinueOnError)
			set.SetOutput(c.Err)
			confirm := set.String("confirm", "", "repeat the database name to confirm")
			if err := set.Parse(pgArgs[1:]); err != nil {
				return err
			}
			if set.NArg() != 1 || *confirm == "" {
				return errors.New("usage: dboss pg drop <database> --confirm <database>")
			}
			request.Method = ops.ActionPGDrop
			request.Database, request.Confirm = set.Arg(0), *confirm
		default:
			return errors.New("usage: dboss pg [backups | backup [database] | delete <backup-id> | restore <backup-id> | drop <database>]")
		}
	case "pubsub":
		pub := opts.rest
		switch {
		case len(pub) > 0 && pub[0] == "help":
			fmt.Fprintln(c.Out, pubsub.Help)
			return nil
		case len(pub) > 0 && pub[0] == "rotate":
			request.Method = ops.ActionPubsubRotate
			set := flag.NewFlagSet("pubsub rotate", flag.ContinueOnError)
			set.SetOutput(c.Err)
			process := set.String("process", "", "web process name when the app serves several hubs")
			flags, positionals := splitFlags(pub[1:], "--process", "-process")
			if err := set.Parse(flags); err != nil {
				return err
			}
			request.Process = *process
			if request.App, err = appArgument(positionals, opts.config); err != nil {
				return fmt.Errorf("usage: dboss pubsub rotate [app] [--process name] (%w)", err)
			}
		case len(pub) > 0 && pub[0] == "secret":
			request.Method = ops.ActionPubsubSecret
			set := flag.NewFlagSet("pubsub secret", flag.ContinueOnError)
			set.SetOutput(c.Err)
			process := set.String("process", "", "web process name when the app serves several hubs")
			flags, positionals := splitFlags(pub[1:], "--process", "-process")
			if err := set.Parse(flags); err != nil {
				return err
			}
			request.Process = *process
			if request.App, err = appArgument(positionals, opts.config); err != nil {
				return fmt.Errorf("usage: dboss pubsub secret [app] [--process name] (%w)", err)
			}
		case len(pub) > 0 && pub[0] == "publish":
			request.Method = ops.ActionPubsubPublish
			set := flag.NewFlagSet("pubsub publish", flag.ContinueOnError)
			set.SetOutput(c.Err)
			event := set.String("event", "message", "event name")
			data := set.String("data", "", `JSON payload, or - to read stdin`)
			process := set.String("process", "", "web process name when the app serves several hubs")
			flags, positionals := splitFlags(pub[1:], "--event", "-event", "--data", "-data", "--process", "-process")
			if err := set.Parse(flags); err != nil {
				return err
			}
			if len(positionals) == 0 {
				return errors.New("usage: dboss pubsub publish [app] <channel> [--event name] [--data json|-] [--process name]")
			}
			request.Channel = positionals[len(positionals)-1]
			if request.App, err = appArgument(positionals[:len(positionals)-1], opts.config); err != nil {
				return fmt.Errorf("usage: dboss pubsub publish <app> <channel> (%w)", err)
			}
			request.Process = *process
			request.Event = *event
			request.Data, err = pubsubData(*data, c.In)
			if err != nil {
				return err
			}
		default:
			request.Method = ops.ActionPubsub
			if len(pub) > 0 {
				if request.App, err = appArgument(pub, opts.config); err != nil {
					return fmt.Errorf("usage: dboss pubsub [app] (%w)", err)
				}
			}
		}
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
	case ops.ActionPG:
		var snapshot pg.Snapshot
		if err := client.Call(request, &snapshot); err != nil {
			return err
		}
		data = snapshot
	case ops.ActionPGBackups:
		var backups []pg.Backup
		if err := client.Call(request, &backups); err != nil {
			return err
		}
		data = backups
	case ops.ActionPGBackup:
		var backups []pg.Backup
		if err := client.Call(request, &backups); err != nil {
			return err
		}
		data = backups
	case ops.ActionPGRestore:
		var result pg.RestoreResult
		if err := client.Call(request, &result); err != nil {
			return err
		}
		data = result
	case ops.ActionPubsub:
		var apps []pubsub.App
		if err := client.Call(request, &apps); err != nil {
			return err
		}
		data = apps
	case ops.ActionPubsubSecret, ops.ActionPubsubRotate:
		var secret ops.PubsubSecret
		if err := client.Call(request, &secret); err != nil {
			return err
		}
		data = secret
	case ops.ActionPubsubPublish:
		var result ops.PubsubPublished
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
