package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
)

type command struct {
	name    string
	args    string
	group   string
	summary string
	details []string
	options []option
}

type option struct{ flag, help string }

var configOption = option{"-c, --config <path>", "config file (default: $APPBOSS_CONFIG, then ./appboss.local.yaml or ./appboss.yaml)"}
var socketOption = option{"--socket <path>", "control socket (default: $APPBOSS_SOCKET, the config's socket when it exists, then /run/appboss/appboss.sock)"}
var jsonOption = option{"--json", "machine-readable output"}
var appArgumentNote = "app defaults to the current folder's app when run inside one."

// commands is the single source for `appboss`, `appboss help <command>` and `<command> --help`.
var commands = []command{
	{name: "start", args: "[-c path]", group: "Host session", summary: "run the host session in the foreground; Ctrl-C stops every app",
		details: []string{"Loads the config, clears every listener in ports.range, starts the apps that were running before, then serves the proxy, the management console and the control socket.", "On a terminal every process's output is echoed with an app/proc prefix. Under systemd only appboss's own log reaches journald; app output stays in log_dir."},
		options: []option{configOption}},
	{name: "systemd", args: "[-c path] [--user name] [--bin path] [--install]", group: "Host session", summary: "print the systemd unit for this config, or install and enable it",
		details: []string{"The unit runs `appboss start -c <absolute config>` as the given user from the config directory with Restart=always and CAP_NET_BIND_SERVICE for port 80."},
		options: []option{configOption, {"--user <name>", "service user (default: current user)"}, {"--bin <path>", "appboss binary (default: this executable)"}, {"--install", "write /etc/systemd/system/appboss.service, reload systemd and enable the service"}}},
	{name: "kill", args: "[-c path]", group: "Host session", summary: "stop every app and terminate every listener left in ports.range",
		details: []string{"Asks the running host to stop each app, then kills whatever still listens in the range. Use it to clean up after a crash or a stray process."},
		options: []option{configOption, jsonOption}},
	{name: "login", args: "", group: "Host session", summary: "print a one-time console URL that signs you in as cli@localhost",
		details: []string{"The link is valid for 3 minutes and works once. It needs management.host to be set in the host config.", "It points at 127.0.0.1 and the console's own port (the first port of ports.range), so it works without DNS. From another machine, tunnel that port first: ssh -L 3100:127.0.0.1:3100 <host>."},
		options: []option{socketOption, configOption, jsonOption}},

	{name: "ls", args: "", group: "Apps", summary: "list apps with state, ports, uptime, last activity and memory",
		details: []string{"STATE shows stopped, starting, running, stopping or crashed, plus `maintenance` while the app answers with the maintenance page."},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "run", args: "[app]", group: "Apps", summary: "start an app; rescans first when it is not known yet",
		details: []string{appArgumentNote, "An app that is stopped is also started automatically by the first proxied request."},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "stop", args: "[app]", group: "Apps", summary: "stop an app and keep it stopped until run or the next request",
		details: []string{appArgumentNote},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "restart", args: "[app]", group: "Apps", summary: "stop and start an app on the same ports",
		details: []string{appArgumentNote, "This is what lux-deploy runs after a release symlink swap."},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "status", args: "[app]", group: "Apps", summary: "full detail for one app: processes, restarts, resources, request rates",
		details: []string{appArgumentNote},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "logs", args: "[app] [-f] [-n lines] [--process name]", group: "Apps", summary: "print or follow the process logs of an app",
		details: []string{appArgumentNote},
		options: []option{{"-f", "follow: keep printing new lines"}, {"-n <lines>", "lines per process (default 200)"}, {"--process <name>", "one process only"}, socketOption, configOption, jsonOption}},
	{name: "maintenance", args: "[app] on|off", group: "Apps", summary: "answer every request with the maintenance page while the app keeps running",
		details: []string{appArgumentNote, "HTML GETs get 503 with the page (maintenance_page, then <static>/503.html, then the built-in one); everything else an empty 503 with Retry-After: 30.", "The flag survives a host restart."},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "cron", args: "[app] | run [app] <job>", group: "Apps", summary: "list an app's scheduled jobs, or run one now",
		details: []string{appArgumentNote, "Jobs are declared under cron: in the app's appboss.yaml, each with a schedule (every 5m, every 2h, every 1d or a 5-field cron expression) and a command. They run in the app folder with the app environment, even while the app is stopped, and their output is written to the log store."},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "hooks", args: "[app] | run [app] <hook> | rotate [app] <hook>", group: "Apps", summary: "list an app's deploy hooks, run one, or rotate its secret",
		details: []string{appArgumentNote, "Hooks are declared under hooks: in the app's appboss.yaml. A signed HTTP POST to <management.url>/hooks/<app>/<hook> starts the hook; a hook with restart: true restarts the app when it exits 0.", "The URL carries a token. With no secret in the config, appboss generates one under state_dir on first use; rotate mints a new one. Every call prints the ready-made ping URL to paste into a Git host webhook."},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "exec", args: "[options] [app] <command> [args...]", group: "Apps", summary: "run a one-off command in the app's environment",
		details: []string{"Runs the command in the app folder with the app environment and prints its combined output. The app argument is optional inside an app folder or when the config there is an app.", "Options must come before the command, so the command's own flags (including -c) pass through untouched. The command is killed after --timeout (default 1m) and its exit code becomes appboss's exit code."},
		options: []option{{"--timeout <duration>", "kill the command after this long (default 1m)"}, socketOption, configOption, jsonOption}},
	{name: "audit", args: "[--app name] [--actor who] [--action name] [-n rows]", group: "Apps", summary: "list operator actions: start, stop, restart, hook runs and config writes",
		details: []string{"Every mutating action records who did what to which app and the result. Console actions carry the signed-in email, hook pings the hook that fired, and control-socket actions are attributed to `cli`.", "Rows are kept for daemon.audit_retention (default 8760h, 0 forever) and pruned with the daily log prune."},
		options: []option{{"--app <name>", "only one app"}, {"--actor <who>", "only one actor"}, {"--action <name>", "only one action"}, {"-n <rows>", "maximum rows (default 200)"}, socketOption, configOption, jsonOption}},

	{name: "config", args: "[app] [-d] | --keys [filter] | --reference | history [app] | restore [app] <revision>", group: "Config", summary: "validate and print a config file, the resolved config, or the key reference",
		details: []string{"Validates first: an unknown key, a bad value or a syntax error is reported with file, line, key and a hint.", "Without -d the file is printed as written, comments included. With -d every default is filled in: the host config, or with an app that app's effective config after the host defaults and its own overrides are merged.", "--keys lists every key with a one-line description and its default, or an example when it has none; a filter narrows by key name. --reference prints the long annotated reference, shipped inside the binary.", "history lists the last 50 saved revisions of the host file or one app's file under state_dir/config-history; restore writes one back. The running host applies it on the next rescan."},
		options: []option{{"-d, --defaults", "print the resolved config with defaults instead of the file as written"}, {"--keys [filter]", "list every configuration key with description and default"}, {"--reference", "print the annotated configuration reference"}, configOption, jsonOption}},
	{name: "check", args: "[-c path]", group: "Config", summary: "validate the config and every app without starting anything",
		details: []string{"Exits 1 and lists each invalid app when something is wrong. Good as a pre-deploy step."},
		options: []option{configOption, jsonOption}},
	{name: "rescan", args: "", group: "Config", summary: "re-read the apps directory, every appboss.yaml and the host defaults",
		details: []string{"App-level changes apply right away. Host keys that changed (proxy, ports, apps, ...) are listed as restart required."},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "ports", args: "", group: "Config", summary: "show the live port table, one fixed port per app process",
		options: []option{socketOption, configOption, jsonOption}},
	{name: "password", args: "", group: "Config", summary: "print a bcrypt hash for basic_auth",
		details: []string{"Prompts without echo on a terminal; reads one line from stdin otherwise, so `printf secret | appboss password` works in scripts."}},
}

func findCommand(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

// wantsHelp reports whether args ask for help of the command they belong to.
func wantsHelp(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "-help" {
			return true
		}
	}
	return false
}

// usage prints the overview: every command grouped, then the shared options.
func (c CLI) usage(out io.Writer) {
	style := newStyle(out)
	fmt.Fprintf(out, "%s runs, proxies and supervises the apps on one host.\n\n", style.bold("appboss"))
	fmt.Fprintf(out, "%s\n  appboss <command> [options]\n  appboss help <command>\n\n", style.heading("Usage"))
	var group string
	width := 0
	for _, cmd := range commands {
		width = max(width, len(cmd.name))
	}
	for _, cmd := range commands {
		if cmd.group != group {
			if group != "" {
				fmt.Fprintln(out)
			}
			group = cmd.group
			fmt.Fprintf(out, "%s\n", style.heading(group))
		}
		fmt.Fprintf(out, "  %s%s   %s\n", style.command(cmd.name), strings.Repeat(" ", width-len(cmd.name)), cmd.summary)
	}
	fmt.Fprintf(out, "\n%s\n", style.heading("Options"))
	writeOptions(out, style, []option{configOption, socketOption, jsonOption})
	fmt.Fprintf(out, "\nRemote commands talk to the running host over its control socket.\n")
	fmt.Fprintf(out, "Run %s for details on one command.\n", style.command("appboss help <command>"))
}

func (c CLI) help(out io.Writer, name string) error {
	cmd := findCommand(name)
	if cmd == nil {
		return fmt.Errorf("unknown command %q (run appboss help)", name)
	}
	style := newStyle(out)
	fmt.Fprintf(out, "%s\n  appboss %s %s\n\n", style.heading("Usage"), style.command(cmd.name), cmd.args)
	fmt.Fprintf(out, "%s.\n", strings.ToUpper(cmd.summary[:1])+cmd.summary[1:])
	for _, line := range cmd.details {
		fmt.Fprintf(out, "\n%s\n", wrap(line, 96))
	}
	if len(cmd.options) > 0 {
		fmt.Fprintf(out, "\n%s\n", style.heading("Options"))
		writeOptions(out, style, cmd.options)
	}
	return nil
}

func writeOptions(out io.Writer, style style, options []option) {
	width := 0
	for _, item := range options {
		width = max(width, len(item.flag))
	}
	for _, item := range options {
		fmt.Fprintf(out, "  %s%s   %s\n", style.command(item.flag), strings.Repeat(" ", width-len(item.flag)), item.help)
	}
}

func wrap(text string, width int) string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		if line != "" && len(line)+1+len(word) > width {
			lines = append(lines, line)
			line = word
			continue
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	return strings.Join(append(lines, line), "\n")
}

// style adds ANSI bold and colour only when the writer is a terminal.
type style struct{ enabled bool }

func newStyle(out io.Writer) style {
	file, ok := out.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" {
		return style{}
	}
	info, err := file.Stat()
	return style{enabled: err == nil && info.Mode()&os.ModeCharDevice != 0}
}

func (s style) bold(text string) string    { return s.wrap("1", text) }
func (s style) heading(text string) string { return s.wrap("1;36", text) }
func (s style) command(text string) string { return s.wrap("36", text) }
func (s style) wrap(code, text string) string {
	if !s.enabled {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}
