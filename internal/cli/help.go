package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
)

type command struct {
	name    string
	aliases []string
	args    string
	group   string
	summary string
	details []string
	options []option
}

// display is the name column: the command plus any aliases, e.g. "start (s)".
func (cmd command) display() string {
	if len(cmd.aliases) == 0 {
		return cmd.name
	}
	return cmd.name + " (" + strings.Join(cmd.aliases, ", ") + ")"
}

type option struct{ flag, help string }

var configOption = option{"-c, --config <path>", "config file (default: $DBOSS_CONFIG, then ./dboss.local.yaml or ./dboss.yaml)"}
var socketOption = option{"--socket <path>", "control socket (default: $DBOSS_SOCKET, the config's socket when it exists, then /run/dboss/dboss.sock)"}
var jsonOption = option{"--json", "machine-readable output"}
var appArgumentNote = "app defaults to the current folder's app when run inside one."

// commands is the single source for `dboss`, `dboss help <command>` and `<command> --help`.
var commands = []command{
	{name: "start", aliases: []string{"s"}, args: "[-c path] [--login] [-y] [--root]", group: "Host session", summary: "run the session in the foreground; Ctrl-C stops every app",
		details: []string{"Loads the config, binds the proxy, the management console and the control socket, then starts the apps that were running before.", "A host clears every listener in ports and counts its ports up from the first one. A dev session (run inside an app folder) shares ports with the dev sessions of other folders: it claims the next free ports from a per-user registry, gets the same ones back on the next start while they are free, and only clears what an earlier run of its own folder left behind.", "A dev session listens on free ports for plain http and HTTPS; --root listens on :80 and :443 instead and fails when either is taken.", "With no config file in the working directory it runs the default host: listen :80, apps in ./apps, state under ./.dboss and the console off.", "On a terminal it first prints the dboss version and one row per process under the key its output is echoed with: the URL to open for a web process, `worker` for the rest, then the console. It then waits for ENTER before starting the apps, so every address can be opened first; a request that arrives meanwhile gets the starting page. -y skips the wait, and so does a start without a terminal.", "Under systemd only dboss's own log reaches journald; app output stays in dir/log, and no banner or console link is printed.", "On a terminal a host's proxy.listen port that needs root or CAP_NET_BIND_SERVICE falls back to the first free port of ports instead of exiting; under systemd the same bind failure is fatal."},
		options: []option{configOption, {"--login", "print a one-time loopback sign-in link for the console (local development)"}, {"-y", "start the apps right away instead of waiting for ENTER"}, {"--root", "dev session: listen on :80 and :443 instead of free ports"}}},
	{name: "systemd", args: "[-c path] [--user name] [--group name] [--bin path] [--install]", group: "Host session", summary: "print the systemd unit for this config, or install and enable it",
		details: []string{"The unit runs `dboss start -c <absolute config>` as the given user from the config directory with Restart=always and CAP_NET_BIND_SERVICE for port 80, so the daemon never runs as root.", "It puts the user's ~/.local/bin first in PATH, which is where mise lives, and hands that user /sys/fs/cgroup/dboss before the daemon starts; without it a non-root daemon falls back to the procgroup backend and the memory and cpu limits are ignored."},
		options: []option{configOption, {"--user <name>", "service user (default: $SUDO_USER under sudo, else the current user)"}, {"--group <name>", "service group (default: the user's primary group)"}, {"--bin <path>", "dboss binary (default: this executable)"}, {"--install", "write /etc/systemd/system/dboss.service, reload systemd and enable the service"}}},
	{name: "kill", args: "[-c path]", group: "Host session", summary: "stop every app and terminate every listener left in ports",
		details: []string{"Asks the running host to stop each app, then kills whatever still listens in the range. Use it to clean up after a crash or a stray process.", "Inside an app folder it only kills what listens on the ports that folder's dev session claimed, so the dev sessions of other folders keep running."},
		options: []option{configOption, jsonOption}},
	{name: "login", args: "", group: "Host session", summary: "print one-time console URLs that sign you in as cli@localhost",
		details: []string{"The links are valid for 3 minutes and work once. They need management.host to be set in the host config.", "It prints a loopback URL on the console's own port (the first port of ports), which needs no DNS, and the public URL on management.host for a direct browser. Both carry the same token, so opening one invalidates the other. Without a public URL, tunnel the port first: ssh -L 3100:127.0.0.1:3100 <host>."},
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
	{name: "destroy", args: "[app]", group: "Apps", summary: "stop and permanently remove an app with deletable: true",
		details: []string{appArgumentNote, "Removes a plain app folder recursively or unlinks an app symlink without following its target. The app must set deletable: true."},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "status", args: "[app]", group: "Apps", summary: "full detail for one app: processes, restarts, resources, request rates",
		details: []string{appArgumentNote},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "logs", args: "[app] [-f] [-n lines] [--process name] [--search q] [--level l] [--channel c]", group: "Apps", summary: "print or follow the process logs of an app, or search the log store",
		details: []string{appArgumentNote, "Without a search flag it tails the live process log files. With --search, --level or --channel it queries the SQLite log store instead: --search is a prefix text match over the message, --level one level, and --channel a channel id such as stdout, request:web or file:production.log."},
		options: []option{{"-f", "follow: keep printing new lines"}, {"-n <lines>", "rows or lines (default 200)"}, {"--process <name>", "one process only"}, {"--search <text>", "search the log store"}, {"--level <level>", "log level filter in store mode"}, {"--channel <id>", "channel filter in store mode"}, socketOption, configOption, jsonOption}},
	{name: "maintenance", args: "[app] on|off", group: "Apps", summary: "answer every request with the maintenance page while the app keeps running",
		details: []string{appArgumentNote, "HTML GETs get 503 with the maintenance page (see dboss pages); everything else an empty 503 with Retry-After: 30.", "The flag survives a host restart."},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "cron", args: "[app] | run [app] <job>", group: "Apps", summary: "list an app's scheduled jobs, or run one now",
		details: []string{appArgumentNote, "Jobs are declared under cron: in the app's dboss.yaml, each with a schedule (every 5m, every 2h, every 1d or a 5-field cron expression) and a command. They run in the app folder with the app environment, even while the app is stopped, and their output is written to the log store."},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "hooks", args: "[app] | run [app] <hook>", group: "Apps", summary: "list an app's deploy hooks with their ping URL, or run one",
		details: []string{appArgumentNote, "Hooks are declared under hooks: in the app's dboss.yaml. A signed HTTP POST to https://<management.host>/hooks/<app>/<hook> starts the hook; a hook with restart: true restarts the app when it exits 0.", "Every ping presents tokens.dboss from the host dboss.yaml: in the URL, as a bearer token, as X-Gitlab-Token, or as the key of GitHub's body signature. Every call prints the ready-made ping URL to paste into a Git host webhook; without tokens.dboss there is none and every ping answers 401."},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "pubsub", args: "[app] | secret [app] | rotate [app] | publish [app] <channel> [--event name] [--data json|-] | help", group: "Apps", summary: "list an app's realtime channels, its publish secret, or publish a message",
		details: []string{appArgumentNote, "Realtime channels are served on the app's own hosts under the web process's pubsub path. A subscriber connects to <path>/<channel> over a WebSocket or SSE; a publisher POSTs the same URL with the publish secret. `dboss pubsub help` prints the browser client and copy-paste examples.", "secret prints the effective publish secret and ready-made URLs; rotate replaces a generated one. A secret set in the config cannot be rotated."},
		options: []option{{"--event <name>", "event name (default message)"}, {"--data <json|->", "JSON payload, or - to read stdin"}, socketOption, configOption, jsonOption}},
	{name: "exec", args: "[options] [app] <command> [args...]", group: "Apps", summary: "run a one-off command in the app's environment",
		details: []string{"Runs the command in the app folder with the app environment and prints its combined output. The app argument is optional inside an app folder or when the config there is an app.", "Options must come before the command, so the command's own flags (including -c) pass through untouched. The command is killed after --timeout (default 1m) and its exit code becomes dboss's exit code."},
		options: []option{{"--timeout <duration>", "kill the command after this long (default 1m)"}, socketOption, configOption, jsonOption}},
	{name: "audit", args: "[--app name] [--actor who] [--action name] [-n rows]", group: "Apps", summary: "list operator actions: start, stop, restart, destroy, hook runs and config writes",
		details: []string{"Every mutating action records who did what to which app and the result. Console actions carry the signed-in email, hook pings the hook that fired, and control-socket actions are attributed to `cli`.", "Rows are kept for audit_retention (default 8760h, 0 forever) and pruned with the daily log prune."},
		options: []option{{"--app <name>", "only one app"}, {"--actor <who>", "only one actor"}, {"--action <name>", "only one action"}, {"-n <rows>", "maximum rows (default 200)"}, socketOption, configOption, jsonOption}},

	{name: "pg", args: "[backups | backup [database] | delete <backup-id> | restore <backup-id> [--target name] [--force] | drop <database> --confirm <database>]", group: "PostgreSQL", summary: "inspect the host PostgreSQL, list backups, run one, delete one, restore one, or drop a database",
		details: []string{"With no argument it prints the server version, connection, uptime, activity and every database with its size and backup rotation.", "backup dumps every selected database; give a name to dump one. delete removes one recorded dump. restore loads a recorded backup into a new database named <source>_restore unless --target names one; replacing an existing database needs --force with --target. drop removes a database and requires its name as --confirm.", "Scheduled backups run daily and are kept for their database's rotation window; a manual backup is kept until you remove it. Dumps live under pg_backup/<database> as a zip of a plain-SQL dump. The server is reached through postgres.dsn, or a local socket and 127.0.0.1 using the PG* environment when it is empty."},
		options: []option{{"--target <name>", "database to restore into (default: <source>_restore_<timestamp>)"}, {"--force", "replace the target database instead of creating a new one"}, {"--confirm <name>", "repeat the database name to drop it"}, socketOption, configOption, jsonOption}},

	{name: "init", args: "[service|app]", group: "Config", summary: "print a fully commented starter config for a service or an app",
		details: []string{"Every key is printed commented out with its default, or an example when it has none, so you uncomment only what you need. With no argument it asks with an up/down menu; pipe input or pass the type to skip it.", "Save it with `dboss init > dboss.yaml` at the host root, or `dboss init app > dboss.yaml` inside an app folder."}},
	{name: "config", args: "[app] [-d] | --keys [filter] | --reference | history [app] | restore [app] <revision>", group: "Config", summary: "validate and print a config file, the resolved config, or the key reference",
		details: []string{"Validates first: an unknown key, a bad value or a syntax error is reported with file, line, key and a hint.", "Without -d the file is printed as written, comments included. With -d every default is filled in: the host config, or with an app that app's effective config after the host defaults and its own overrides are merged.", "--keys lists every key with a one-line description and its default, or an example when it has none; a filter narrows by key name. --reference prints the long annotated reference, shipped inside the binary.", "history lists the last 50 saved revisions of the host file or one app's file under dir/state/config-history; restore writes one back. The running host applies it on the next rescan."},
		options: []option{{"-d, --defaults", "print the resolved config with defaults instead of the file as written"}, {"--keys [filter]", "list every configuration key with description and default"}, {"--reference", "print the annotated configuration reference"}, configOption, jsonOption}},
	{name: "pages", args: "[app] | dump [app] [name...] [--all] [--force]", group: "Config", summary: "list the pages dboss serves and which file renders each, or write them out to edit",
		details: []string{"dboss answers some requests itself: starting, stopped, crashed, maintenance, error, forbidden, signed_out, and on the host 404 and login. Each is <name>.html in the pages folder (pages:, default ./public/error_pages), else template.html there, else the host's, else the built-in template.", "dump writes template.html, or the named pages with their wording written in, into the app's pages folder (or the host's without an app). --all writes the template and every page. An existing file is kept unless --force.", "Pages fill {{status}}, {{title}}, {{message}}, {{action}}, {{app}} and {{dboss_logo}}; any other {{...}} is left alone."},
		options: []option{{"--all", "dump the template and every page"}, {"--force", "overwrite existing files"}, configOption, jsonOption}},
	{name: "check", args: "[-c path]", group: "Config", summary: "validate the config and every app without starting anything",
		details: []string{"Exits 1 and lists each invalid app when something is wrong. Good as a pre-deploy step."},
		options: []option{configOption, jsonOption}},
	{name: "doctor", args: "[-c path]", group: "Config", summary: "preflight a box: tools, writable directories, valid config and a clear port range",
		details: []string{"Checks that lsof is on PATH, that dir and its state and log folders are writable, that the config and every app load, and whether anything still listens in ports. Warns on listeners a start would clear; fails on anything that would stop the session."},
		options: []option{configOption, jsonOption}},
	{name: "rescan", args: "", group: "Config", summary: "re-read the apps directory, every dboss.yaml and the host defaults",
		details: []string{"App-level changes apply right away. Host keys that changed (proxy, ports, apps, ...) are listed as restart required."},
		options: []option{socketOption, configOption, jsonOption}},
	{name: "ports", args: "", group: "Config", summary: "show the live port table, one fixed port per app process",
		options: []option{socketOption, configOption, jsonOption}},
	{name: "password", args: "", group: "Config", summary: "print a bcrypt hash for basic_auth",
		details: []string{"Prompts without echo on a terminal; reads one line from stdin otherwise, so `printf secret | dboss password` works in scripts."}},
	{name: "trust", args: "", group: "Config", summary: "trust the local certificate authority behind a dev session's HTTPS",
		details: []string{"A dev session (`dboss start` in an app folder) also serves HTTPS, on a free port or on :443 with --root, with certificates from one local authority per user, kept in the user config directory and shared by every project.", "trust adds that root to the login keychain on macOS (it asks for your password) or to the system store through sudo on Debian and Fedora. Anywhere else it prints the file to import by hand. It does nothing when the root is already trusted."}},
	{name: "sshkey", args: "[list [--dir path] [--json]] | new [name] [-t type] [-b bits] [-C comment] [-f path] [--no-passphrase] [--force]", group: "Config", summary: "list the local SSH public keys, or create a new key",
		details: []string{"list prints every *.pub in ~/.ssh as name plus the public key value, ready to paste into GitHub or GitLab. --json adds the type, bits, fingerprint, comment and whether the private key exists next to it.", "new runs ssh-keygen, ed25519 by default, then prints the public value and the GitHub and GitLab add pages. It refuses to overwrite an existing key without --force."},
		options: []option{{"--dir <path>", "key directory (default ~/.ssh)"}, {"-t <type>", "ed25519 (default), rsa or ecdsa"}, {"-b <bits>", "key bits (default 4096 rsa, 521 ecdsa)"}, {"-C <comment>", "key comment (default user@host)"}, {"-f <path>", "target path (default <dir>/<name>)"}, {"--no-passphrase", "create without a passphrase"}, {"--force", "overwrite an existing key"}, jsonOption}},

	{name: "version", args: "", group: "Binary", summary: "print the dboss version",
		details: []string{"The version is the number of commits in main when the binary was built, printed as v<count>. There is nothing else to it: no major.minor.patch, and the release tag carries the same number.", "A binary built straight from source with `go build` reports dev, since only `make build` and the release workflow inject the count."}},
	{name: "update", args: "[--check] [--version tag] [--force]", group: "Binary", summary: "download and install the latest dboss release",
		details: []string{"Resolves the latest release on GitHub, compares it with this binary, downloads the asset for this platform, verifies its sha256 against the release checksums and replaces the running executable. A failed check leaves the old binary untouched.", "It follows a symlink to the real file, so `~/bin/dboss` updates what it points at. When the binary directory is not writable it stops and asks for `sudo dboss update`; it never elevates itself.", "The running host keeps the old binary in memory until it is restarted."},
		options: []option{{"--check", "report whether a newer release exists, then exit"}, {"--version <tag>", "install a named release instead of the latest"}, {"--force", "install over a from-source build or the same version"}, jsonOption}},
}

func findCommand(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
		for _, alias := range commands[i].aliases {
			if alias == name {
				return &commands[i]
			}
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
	fmt.Fprintf(out, "%s runs, proxies and supervises the apps on one host.\n\n", style.bold("dboss"))
	fmt.Fprintf(out, "%s\n  dboss <command> [options]\n  dboss help <command>\n\n", style.heading("Usage"))
	var group string
	width := 0
	for _, cmd := range commands {
		width = max(width, len(cmd.display()))
	}
	for _, cmd := range commands {
		if cmd.group != group {
			if group != "" {
				fmt.Fprintln(out)
			}
			group = cmd.group
			fmt.Fprintf(out, "%s\n", style.heading(group))
		}
		name := cmd.display()
		fmt.Fprintf(out, "  %s%s   %s\n", style.command(name), strings.Repeat(" ", width-len(name)), cmd.summary)
	}
	fmt.Fprintf(out, "\n%s\n", style.heading("Options"))
	writeOptions(out, style, []option{configOption, socketOption, jsonOption})
	fmt.Fprintf(out, "\nRemote commands talk to the running host over its control socket.\n")
	fmt.Fprintf(out, "Run %s for details on one command.\n", style.command("dboss help <command>"))
}

func (c CLI) help(out io.Writer, name string) error {
	cmd := findCommand(name)
	if cmd == nil {
		return fmt.Errorf("unknown command %q (run dboss help)", name)
	}
	style := newStyle(out)
	fmt.Fprintf(out, "%s\n  dboss %s %s\n\n", style.heading("Usage"), style.command(cmd.name), cmd.args)
	if len(cmd.aliases) > 0 {
		fmt.Fprintf(out, "Alias: %s\n\n", strings.Join(cmd.aliases, ", "))
	}
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
