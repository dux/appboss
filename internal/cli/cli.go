package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"deploy-boss/internal/apps"
	"deploy-boss/internal/config"
	"deploy-boss/internal/console"
	"deploy-boss/internal/ctl"
	"deploy-boss/internal/ports"
	"deploy-boss/internal/proxy"
	"deploy-boss/internal/reqlog"
	"deploy-boss/internal/super"
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
	if findCommand(command) == nil {
		fmt.Fprintf(c.Err, "dboss: unknown command %q\n\n", command)
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
	case "config", "check", "kill":
		err = c.local(command, args[1:])
	default:
		err = c.remote(command, args[1:])
	}
	if err != nil {
		fmt.Fprintln(c.Err, "dboss:", err)
		return 1
	}
	return 0
}

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
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: dboss start [-c path]")
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
	for _, dir := range []string{cfg.StateDir, cfg.LogDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	cleared, err := super.ClearPortRange(cfg.Ports.Range, cfg.Defaults.StopTimeout.Value())
	if err != nil {
		return err
	}
	if len(cleared) > 0 {
		log.Printf("cleared app port range %d-%d: pids=%v", cfg.Ports.Range[0], cfg.Ports.Range[1], cleared)
	}
	allocator, managementPort := newAllocator(cfg)
	manager, invalid, err := super.New(cfg, allocator, echo)
	if err != nil {
		return err
	}
	defer manager.Close()
	for _, scanErr := range invalid {
		log.Printf("skip invalid app: %v", scanErr)
	}
	requestLogs := reqlog.New(cfg.LogDir, cfg.Defaults.LogFlush.Value())
	defer requestLogs.Close()
	var edge http.Handler
	var management *console.Handler
	if len(cfg.Proxy.Listen) > 0 {
		if edge, management, err = edgeHandler(cfg, manager, requestLogs); err != nil {
			return err
		}
	}
	var login func() (string, error)
	if management != nil {
		login = management.LoginURL
	}
	control, err := ctl.Listen(cfg.Socket, manager, requestLogs, login)
	if err != nil {
		return err
	}
	defer control.Close()
	var servers []*http.Server
	if edge != nil {
		for _, address := range cfg.Proxy.Listen {
			server, err := startHTTPServer("proxy", address, edge)
			if err != nil {
				return err
			}
			defer server.Close()
			servers = append(servers, server)
		}
		if management != nil {
			server, err := startHTTPServer("management", managementAddress(managementPort), management)
			if err != nil {
				return err
			}
			defer server.Close()
			servers = append(servers, server)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go pruneLoop(ctx, requestLogs, manager, cfg.Daemon.PruneAt)
	log.Printf("dboss ready: config=%s socket=%s listen=%s management=%s port=%d", cfg.SourcePath, cfg.Socket, strings.Join(cfg.Proxy.Listen, ","), strings.Join(cfg.Management.Host, ","), managementPort)
	<-ctx.Done()
	for _, server := range servers {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = server.Shutdown(shutdown)
		cancel()
	}
	return nil
}

// newAllocator reserves the first port of the range for the management console before any app
// is discovered, so app ports never shift when the console is turned on or off.
func newAllocator(cfg config.Config) (*ports.Allocator, int) {
	allocator := ports.New(cfg.Ports.Range)
	port, _ := allocator.Allocate("dboss", "management")
	return allocator, port
}

func managementAddress(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

// edgeHandler is the single public listener: Cloudflare hands it the full request and the
// host header picks the console or an app. Only the app proxy is affected by the trusted CIDRs.
// The console handler is returned as well so it can be served on its own port and mint
// login links; it is nil when the console is not enabled.
func edgeHandler(cfg config.Config, manager *super.Manager, requestLogs *reqlog.Manager) (http.Handler, *console.Handler, error) {
	appProxy, err := proxy.New(cfg, manager, requestLogs)
	if err != nil {
		return nil, nil, err
	}
	var handler http.Handler = appProxy
	var management *console.Handler
	if cfg.Management.Enabled() {
		management, err = console.New(cfg, manager, requestLogs, apps.NewStore(cfg))
		if err != nil {
			return nil, nil, fmt.Errorf("management console: %w", err)
		}
		handler = proxy.HostSwitch(cfg.Management.Host, management, appProxy)
	}
	edge, err := proxy.TrustedOnly(cfg.Proxy.TrustedCIDRs, handler)
	return edge, management, err
}

func startHTTPServer(name, address string, handler http.Handler) (*http.Server, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		if errors.Is(err, syscall.EACCES) {
			return nil, fmt.Errorf("%s listen %s: %w (port needs CAP_NET_BIND_SERVICE: run the systemd unit, or set proxy.listen to a high port for a hand-run session)", name, address, err)
		}
		return nil, fmt.Errorf("%s listen: %w", name, err)
	}
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("%s: %v", name, err)
		}
	}()
	return server, nil
}

func pruneLoop(ctx context.Context, logs *reqlog.Manager, manager *super.Manager, at string) {
	for {
		now := time.Now()
		target, _ := time.ParseInLocation("15:04", at, now.Location())
		next := time.Date(now.Year(), now.Month(), now.Day(), target.Hour(), target.Minute(), 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			for _, snapshot := range manager.Snapshots() {
				if err := logs.PruneApp(ctx, snapshot.Name, snapshot.LogRetention); err != nil {
					log.Printf("request log prune %s: %v", snapshot.Name, err)
				}
			}
		}
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
	path, err := findConfig(*pathFlag)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
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
	case "run", "stop", "restart", "status":
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
		if err := set.Parse(opts.rest); err != nil {
			return err
		}
		if set.NArg() != 0 {
			return errors.New("usage: dboss logs [app] [-f] [-n 200] [--process name]")
		}
		request.Lines, request.Process = *lines, *processName
		if *follow && opts.json {
			return errors.New("--json and -f cannot be combined")
		}
		if *follow {
			return c.follow(client, request)
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
	case "ports":
		var entries map[string]int
		if err := client.Call(request, &entries); err != nil {
			return err
		}
		data = entries
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

var keyGroups = []struct{ id, title, note string }{
	{config.GroupHost, "Host keys", "dboss.yaml with apps:"},
	{config.GroupApp, "App keys", "dboss.yaml with procfile:"},
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
				values = append(values, fmt.Sprintf("%s:%d", process.Name, process.Port))
			}
			last := "-"
			if !snapshot.LastActivity.IsZero() {
				last = snapshot.LastActivity.Format(time.RFC3339)
			}
			state := string(snapshot.State)
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
		names := make([]string, 0, len(logs))
		for name := range logs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, line := range logs[name] {
				fmt.Fprintf(c.Out, "[%s] %s\n", name, line)
			}
		}
	case "ports":
		entries := data.(map[string]int)
		names := make([]string, 0, len(entries))
		for name := range entries {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(c.Out, "%s\t%d\n", name, entries[name])
		}
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
		names := make([]string, 0, len(logs))
		for name := range logs {
			names = append(names, name)
		}
		sort.Strings(names)
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
