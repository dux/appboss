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

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/ctl"
	"dboss/internal/daemon"
	"dboss/internal/devtls"
	"dboss/internal/ops"
	"dboss/internal/super"
	"dboss/internal/version"
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

// start runs the host session in the foreground. Under systemd this is the service process;
// on a terminal every app's output is echoed with an app/proc prefix.
func (c CLI) start(args []string) error {
	set := flag.NewFlagSet("start", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := configFlag(set)
	login := set.Bool("login", false, "print a one-time console sign-in link")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: dboss start [-c path] [--login]")
	}
	cfg, err := loadHostConfig(*configPath)
	if err != nil {
		return err
	}
	var echo *super.Echo
	if info, statErr := os.Stdout.Stat(); statErr == nil && info.Mode()&os.ModeCharDevice != 0 {
		echo = super.NewEcho(c.Out)
		if cfg.Dev() {
			echo.Solo()
		}
		warnUnignoredRuntime(c.Err, cfg)
		if cfg.Dev() && len(cfg.Proxy.Listen) > 0 {
			c.offerTrust()
		}
	}
	session, err := daemon.Build(cfg, echo)
	if err != nil {
		return err
	}
	defer session.Close()
	if *login {
		// stdout only: the token must never reach the daemon log
		local, _, err := session.LoginURL()
		if err != nil {
			return err
		}
		fmt.Fprintf(c.Out, "login: %s (one-time, 3 minutes)\n", local)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return session.Run(ctx)
}

// trust adds the dev certificate authority's root to the system trust store, so the browser
// accepts the HTTPS a dev session serves. It creates the root first when there is none yet.
func (c CLI) trust(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: dboss trust")
	}
	dir, err := devtls.DefaultDir()
	if err != nil {
		return err
	}
	authority, err := devtls.Open(dir)
	if err != nil {
		return err
	}
	if authority.Trusted() {
		fmt.Fprintf(c.Out, "already trusted: %s\n", authority.RootPath())
		return nil
	}
	return c.installTrust(authority)
}

func (c CLI) installTrust(authority *devtls.Authority) error {
	fmt.Fprintf(c.Out, "adding %s to the system trust store\n", authority.RootPath())
	if err := authority.Install(c.Out); err != nil {
		return err
	}
	if !authority.Trusted() {
		return fmt.Errorf("the root was added but the system still does not trust it; import %s by hand", authority.RootPath())
	}
	fmt.Fprintln(c.Out, "trusted: restart the browser if it was open; Firefox may need the root imported in its own settings")
	return nil
}

// offerTrust asks, before a hand-run dev session starts serving, whether to trust the local
// certificate authority its HTTPS uses. Declining or failing never stops the start; the banner
// keeps saying the certificate is not trusted.
func (c CLI) offerTrust() {
	in, ok := c.In.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return
	}
	dir, err := devtls.DefaultDir()
	if err != nil {
		return
	}
	authority, err := devtls.Open(dir)
	if err != nil || authority.Trusted() {
		return
	}
	fmt.Fprint(c.Out, "HTTPS in this dev session uses a local certificate authority this machine does not trust yet,\nso browsers will warn. Trust it now? It may ask for your password. [Y/n] ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	if answer = strings.ToLower(strings.TrimSpace(answer)); answer != "" && answer != "y" && answer != "yes" {
		fmt.Fprintln(c.Out, "starting without it; run `dboss trust` any time")
		return
	}
	if err := c.installTrust(authority); err != nil {
		fmt.Fprintf(c.Err, "dboss: trust: %v; starting anyway\n", err)
	}
}

// password prints a bcrypt hash for basic_auth. The prompt hides input on a terminal; piped
// input is read as one line so the hash can be scripted.
func (c CLI) password(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: dboss password")
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

// init prints a fully commented starter config for a service (the root dboss.yaml) or an app.
// With no argument it asks which one to generate, defaulting to service.
func (c CLI) init(args []string) error {
	if len(args) > 1 {
		return errors.New("usage: dboss init [service|app]")
	}
	role := ""
	if len(args) == 1 {
		role = templateRole(args[0])
		if role == "" {
			return fmt.Errorf("unknown config type %q (use service or app)", args[0])
		}
	} else {
		selected, err := c.selectTemplateRole()
		if err != nil {
			return err
		}
		role = selected
	}
	template, err := config.Template(role)
	if err != nil {
		return err
	}
	_, err = io.WriteString(c.Out, template)
	return err
}

// askTemplateRole prompts for the config type and defaults to service on an empty answer. It
// reads one line, so `printf '2\n' | dboss init` works without a terminal.
func (c CLI) askTemplateRole() (string, error) {
	fmt.Fprint(c.Err, "Generate config for:\n  1) service (root dboss.yaml)\n  2) app (an app's dboss.yaml)\nSelect [1]: ")
	line, err := bufio.NewReader(c.In).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return config.TemplateService, nil
	}
	role := templateRole(answer)
	if role == "" {
		return "", fmt.Errorf("unknown selection %q (use 1 or 2)", answer)
	}
	return role, nil
}

// selectTemplateRole shows an arrow-key menu on a terminal; when input or output is not a
// terminal it falls back to the typed prompt so scripts and tests still work.
func (c CLI) selectTemplateRole() (string, error) {
	in, ok := c.In.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return c.askTemplateRole()
	}
	out := c.Err
	if file, ok := c.Err.(*os.File); !ok || !term.IsTerminal(int(file.Fd())) {
		file, ok := c.Out.(*os.File)
		if !ok || !term.IsTerminal(int(file.Fd())) {
			return c.askTemplateRole()
		}
		out = file
	}
	labels := []string{"service (root dboss.yaml)", "app (an app's dboss.yaml)"}
	roles := []string{config.TemplateService, config.TemplateApp}
	selected := 0
	draw := func(first bool) {
		if !first {
			fmt.Fprint(out, "\x1b[2A")
		}
		for i, label := range labels {
			marker := "  "
			if i == selected {
				marker = "> "
			}
			fmt.Fprintf(out, "\r\x1b[2K%s%s\n", marker, label)
		}
	}
	fmt.Fprint(out, "Generate config for (up/down, Enter):\n")
	draw(true)
	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return "", err
	}
	defer term.Restore(int(in.Fd()), state)
	for {
		key, err := readKey(in)
		if err != nil {
			return "", err
		}
		switch key {
		case "up":
			if selected > 0 {
				selected--
				draw(false)
			}
		case "down":
			if selected < len(labels)-1 {
				selected++
				draw(false)
			}
		case "1", "2", "enter":
			if key == "1" {
				selected = 0
			}
			if key == "2" {
				selected = 1
			}
			fmt.Fprintf(out, "\x1b[2A\r\x1b[2K%s\n\r\x1b[2K", labels[selected])
			return roles[selected], nil
		case "cancel":
			fmt.Fprint(out, "\x1b[2A\r\x1b[2K\r\x1b[2K")
			return "", errors.New("cancelled")
		}
	}
}

// readKey reads one key in raw mode, translating arrows to up/down and Enter to enter. It
// returns "" for keys it does not use.
func readKey(in *os.File) (string, error) {
	buf := make([]byte, 1)
	if _, err := in.Read(buf); err != nil {
		return "", err
	}
	switch buf[0] {
	case '\r', '\n':
		return "enter", nil
	case 0x03, 'q':
		return "cancel", nil
	case 'k':
		return "up", nil
	case 'j':
		return "down", nil
	case '1':
		return "1", nil
	case '2':
		return "2", nil
	case 0x1b:
		sequence := make([]byte, 2)
		if _, err := io.ReadFull(in, sequence); err != nil {
			return "", err
		}
		if sequence[0] == '[' {
			switch sequence[1] {
			case 'A':
				return "up", nil
			case 'B':
				return "down", nil
			}
		}
	}
	return "", nil
}

// templateRole maps an argument or prompt answer to a template role, "" when unknown.
func templateRole(answer string) string {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "1", "service", "s":
		return config.TemplateService
	case "2", "app", "a":
		return config.TemplateApp
	}
	return ""
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
			return errors.New("usage: dboss config --keys [filter]")
		}
		return c.printKeys(set.Arg(0), *jsonOutput)
	}
	var cfg config.Config
	var err error
	if command == "config" {
		path, pathErr := findConfig(*pathFlag)
		if pathErr != nil {
			return pathErr
		}
		if cfg, err = config.Load(path); err != nil {
			return err
		}
	} else {
		if cfg, err = loadHostConfig(*pathFlag); err != nil {
			return err
		}
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
			return errors.New("usage: dboss doctor [-c path]")
		}
		return c.doctor(cfg, *jsonOutput)
	}
	if command == "kill" {
		if set.NArg() != 0 {
			return errors.New("usage: dboss kill [-c path]")
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
		return errors.New("usage: dboss config [app] [-d|--defaults]")
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
		return errors.New("usage: dboss config restore [app] <revision>")
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
	fmt.Fprintf(c.Out, "restored %s to revision %s; run `dboss rescan` to apply it\n", file.ID, revision)
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
		return "", errors.New("usage: dboss config history|restore [app]")
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
		add("fail", "lsof is not on PATH; dboss needs it to clear the port range")
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
	listeners, listenErr := super.ListenersInRange(cfg.Ports.Range)
	switch {
	case listenErr != nil:
		add("warn", "port range check failed: "+listenErr.Error())
	case len(listeners) > 0:
		pids := make([]int, 0, len(listeners))
		for _, listener := range listeners {
			if len(pids) == 0 || pids[len(pids)-1] != listener.PID {
				pids = append(pids, listener.PID)
			}
		}
		add("warn", fmt.Sprintf("port range %d-%d has listeners (pids %v); a start clears them", cfg.Ports.Range[0], cfg.Ports.Range[1], pids))
	default:
		add("ok", fmt.Sprintf("port range %d-%d is clear", cfg.Ports.Range[0], cfg.Ports.Range[1]))
	}
	// An app binds its own port and dboss only ever dials 127.0.0.1, so a listener on a public
	// address answers without the proxy in front of it. dboss's own listeners are skipped: the
	// console is loopback by design and a hand-run proxy may take a port from the range.
	for _, listener := range listeners {
		if listener.Loopback() || listener.Command == "dboss" {
			continue
		}
		add("warn", fmt.Sprintf("%s (pid %d) listens on %s: that port answers without the proxy, so basic_auth, allow_ips, the sign-in gate and the X-Dboss-User strip do not apply; bind 127.0.0.1 or firewall %d-%d",
			listener.Command, listener.PID, listener.Address, cfg.Ports.Range[0], cfg.Ports.Range[1]))
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
	file, err := os.CreateTemp(dir, ".dboss-doctor-*")
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

// flagsFirst moves flags ahead of positional arguments so `dboss config app -d` and
// `dboss config --keys static --json` parse the same as with the flags in front.
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

// splitFlags separates flags from positional arguments so a value flag may appear after an
// app or channel argument, which Go's flag package does not allow. valued names the flags that
// take a following value.
func splitFlags(args []string, valued ...string) (flags, positionals []string) {
	set := map[string]bool{}
	for _, name := range valued {
		set[name] = true
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name := arg
		if index := strings.IndexByte(arg, '='); index >= 0 {
			name = arg[:index]
		}
		if set[name] {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flags = append(flags, arg)
			continue
		}
		positionals = append(positionals, arg)
	}
	return flags, positionals
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

const defaultSocket = "/run/dboss/dboss.sock"

// findSocket resolves the control socket: --socket, DBOSS_SOCKET, the socket of the config in
// reach if it exists on disk, then the well-known production path. The last step is what lets
// `dboss restart` inside a deployed app folder reach the host session started elsewhere.
func findSocket(explicit, configPath string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("DBOSS_SOCKET"); env != "" {
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

// loadHostConfig loads the config file, or, when none was requested explicitly and none exists in
// the working directory, synthesizes the default host for it. That is what makes `start`, `check`
// and `doctor` work in a folder that only has an apps/ directory.
func loadHostConfig(explicit string) (config.Config, error) {
	path, err := findConfig(explicit)
	if err != nil {
		if explicit != "" || os.Getenv("DBOSS_CONFIG") != "" || !errors.Is(err, config.ErrNoConfig) {
			return config.Config{}, err
		}
		dir, wdErr := os.Getwd()
		if wdErr != nil {
			return config.Config{}, err
		}
		return config.Parse(nil, filepath.Join(dir, config.FileName))
	}
	return config.Load(path)
}

func findConfig(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("DBOSS_CONFIG"); env != "" {
		return env, nil
	}
	path, err := config.FindInDir(".")
	if err != nil {
		return "", fmt.Errorf("%w (use -c or DBOSS_CONFIG)", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return absolute, nil
}
