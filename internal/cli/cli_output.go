package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"dboss/internal/config"
	"dboss/internal/logstore"
	"dboss/internal/ops"
	"dboss/internal/pg"
	"dboss/internal/pubsub"
	"dboss/internal/super"
)

// printKeys lists the documented config keys, grouped by block. filter narrows by a substring
// of the key path.
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
	for _, block := range config.Blocks() {
		first := true
		for _, key := range keys {
			if key.Block != block.ID {
				continue
			}
			if first {
				fmt.Fprintf(writer, "%s  (%s)\n", block.Title, blockNote(block.Scope))
				first = false
			}
			value := key.Default
			if value == "" {
				value = "e.g. " + key.Example
			} else if key.Example != "" {
				value = value + "  e.g. " + key.Example
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

// blockNote says which file a block belongs in, printed next to the block title.
func blockNote(scope config.Scope) string {
	switch scope {
	case config.ScopeService:
		return "root dboss.yaml"
	case config.ScopeApp:
		return "app dboss.yaml"
	default:
		return "defaults: in the root file, top level in an app file; per-process ones also under processes.<name>"
	}
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
			fmt.Fprintf(c.Out, "restart required: %s changed (systemctl restart dboss, or Ctrl-C and dboss start)\n", joinAny(keys))
		}
	case "login":
		links := data.(map[string]string)
		fmt.Fprintf(c.Out, "local:  %s\n", links["url"])
		if public := links["public_url"]; public != "" {
			fmt.Fprintf(c.Out, "public: %s\n", public)
		}
		fmt.Fprintln(c.Out, "Opens the console as cli@localhost. Valid for 3 minutes, one use.")
	case ops.ActionPG:
		snapshot := data.(pg.Snapshot)
		summary := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintf(summary, "server\t%s\n", snapshot.Server.Version)
		fmt.Fprintf(summary, "connection\t%s\n", snapshot.Server.Description)
		fmt.Fprintf(summary, "uptime\t%d seconds\n", snapshot.Server.UptimeSeconds)
		fmt.Fprintf(summary, "connections\t%d of %d\n", snapshot.Server.CurrentConnections, snapshot.Server.MaxConnections)
		fmt.Fprintf(summary, "activity\t%d active, %d idle, %d blocked\n", snapshot.Activity.Active, snapshot.Activity.Idle, snapshot.Activity.Blocked)
		if err := summary.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(c.Out)
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "DATABASE\tSIZE\tOWNER\tCONNS\tBACKUP\tROTATION")
		for _, database := range snapshot.Databases {
			fmt.Fprintf(writer, "%s\t%d\t%s\t%d\t%t\t%s\n", database.Name, database.SizeBytes, database.Owner, database.Connections, database.BackupSelected, database.BackupRotation)
		}
		return writer.Flush()
	case ops.ActionPGBackups:
		backups := data.([]pg.Backup)
		if len(backups) == 0 {
			fmt.Fprintln(c.Out, "no backups recorded")
			return nil
		}
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "DATABASE\tTIME\tSIZE\tMANUAL\tSTATUS\tID")
		for _, entry := range backups {
			fmt.Fprintf(writer, "%s\t%s\t%d\t%t\t%s\t%s\n", entry.Database, entry.Time, entry.Bytes, entry.Manual, entry.Status, entry.ID)
		}
		return writer.Flush()
	case ops.ActionPGBackup:
		backups := data.([]pg.Backup)
		for _, entry := range backups {
			if entry.Error != "" {
				fmt.Fprintf(c.Out, "%s: failed: %s\n", entry.Database, entry.Error)
				continue
			}
			fmt.Fprintf(c.Out, "%s: %d bytes in %dms\n", entry.Database, entry.Bytes, entry.DurationMS)
		}
	case ops.ActionPGRestore:
		result := data.(pg.RestoreResult)
		fmt.Fprintf(c.Out, "restored %d bytes into %s\n", result.Bytes, result.Target)
	case ops.ActionPGDrop:
		fmt.Fprintf(c.Out, "dropped %s\n", data.(string))
	case ops.ActionPubsub:
		apps := data.([]pubsub.App)
		if len(apps) == 0 {
			fmt.Fprintln(c.Out, "no app serves realtime channels (set pubsub on an app's web process)")
			return nil
		}
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "APP\tPROCESS\tPATH\tCLIENTS\tCHANNELS")
		for _, app := range apps {
			channels := "-"
			if len(app.Channels) > 0 {
				parts := make([]string, len(app.Channels))
				for i, channel := range app.Channels {
					parts[i] = fmt.Sprintf("%s(%d)", channel.Name, channel.Subscribers)
				}
				channels = strings.Join(parts, ",")
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\n", app.Name, app.Process, app.Path, app.Clients, channels)
		}
		return writer.Flush()
	case ops.ActionPubsubSecret, ops.ActionPubsubRotate:
		info := data.(ops.PubsubSecret)
		if method == ops.ActionPubsubRotate {
			fmt.Fprintln(c.Out, "rotated; new publish secret:")
		}
		fmt.Fprintf(c.Out, "app:       %s\n", info.App)
		fmt.Fprintf(c.Out, "process:   %s\n", info.Process)
		fmt.Fprintf(c.Out, "path:      %s\n", info.Path)
		fmt.Fprintf(c.Out, "secret:    %s\n", info.Secret)
		fmt.Fprintf(c.Out, "subscribe: %s\n", info.Subscribe)
		fmt.Fprintf(c.Out, "publish:   %s\n", info.Publish)
	case ops.ActionPubsubPublish:
		result := data.(ops.PubsubPublished)
		fmt.Fprintf(c.Out, "published to %s (%d subscribers)\n", result.Channel, result.Subscribers)
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
