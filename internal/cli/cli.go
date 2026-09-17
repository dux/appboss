package cli

import (
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
	"gopkg.in/yaml.v3"
)

type CLI struct {
	Out io.Writer
	Err io.Writer
}

func (c CLI) Run(args []string) int {
	if c.Out == nil {
		c.Out = os.Stdout
	}
	if c.Err == nil {
		c.Err = os.Stderr
	}
	if len(args) == 0 {
		c.usage()
		return 2
	}
	command := args[0]
	if command == "daemon" {
		if err := c.daemon(args[1:]); err != nil {
			fmt.Fprintln(c.Err, "dboss:", err)
			return 1
		}
		return 0
	}
	if command == "config" || command == "check" || command == "kill" {
		if err := c.local(command, args[1:]); err != nil {
			fmt.Fprintln(c.Err, "dboss:", err)
			return 1
		}
		return 0
	}
	jsonOutput, socket, remaining, err := commonArgs(args[1:])
	if err != nil {
		fmt.Fprintln(c.Err, "dboss:", err)
		return 2
	}
	if socket == "" {
		socket = os.Getenv("BOSS_SOCKET")
	}
	if socket == "" {
		socket = "/run/boss/boss.sock"
	}
	if err := c.remote(command, remaining, jsonOutput, socket); err != nil {
		fmt.Fprintln(c.Err, "dboss:", err)
		return 1
	}
	return 0
}

func (c CLI) usage() {
	fmt.Fprintln(c.Err, "usage: dboss daemon|config|check|kill|ls|status|start|stop|restart|rescan|logs|ports [options]")
}

func (c CLI) daemon(args []string) error {
	set := flag.NewFlagSet("daemon", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := set.String("config", "", "config file")
	stateDir := set.String("state-dir", "", "state directory override")
	logDir := set.String("log-dir", "", "log directory override")
	socket := set.String("socket", "", "control socket override")
	_ = set.Bool("json", false, "accepted for command consistency")
	if err := set.Parse(args); err != nil {
		return err
	}
	path, err := findConfig(*configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	var daemonLog *os.File
	if cfg.Daemon.Log != "stderr" {
		if err := os.MkdirAll(filepath.Dir(cfg.Daemon.Log), 0o750); err != nil {
			return err
		}
		daemonLog, err = os.OpenFile(cfg.Daemon.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			return err
		}
		defer daemonLog.Close()
		previousLog := log.Writer()
		defer log.SetOutput(previousLog)
		log.SetOutput(daemonLog)
	}
	if *stateDir != "" {
		cfg.StateDir = *stateDir
	}
	if *logDir != "" {
		cfg.LogDir = *logDir
	}
	if *socket != "" {
		cfg.Socket = *socket
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
	manager, invalid, err := super.New(cfg, ports.New(cfg.Ports.Range))
	if err != nil {
		return err
	}
	defer manager.Close()
	for _, scanErr := range invalid {
		log.Printf("skip invalid app: %v", scanErr)
	}
	requestLogs := reqlog.New(cfg.LogDir, cfg.Defaults.LogFlush.Value())
	defer requestLogs.Close()
	control, err := ctl.Listen(cfg.Socket, manager, requestLogs)
	if err != nil {
		return err
	}
	defer control.Close()
	servers := make([]*http.Server, 0, 2)
	defer func() {
		for _, server := range servers {
			_ = server.Close()
		}
	}()
	if cfg.Proxy.Listen != "" {
		handler, err := proxy.New(cfg, manager, requestLogs)
		if err != nil {
			return err
		}
		server, err := startHTTPServer("proxy", cfg.Proxy.Listen, handler)
		if err != nil {
			return err
		}
		servers = append(servers, server)
	}
	if cfg.Management.Listen != "" {
		handler, err := console.New(cfg, manager, requestLogs)
		if err != nil {
			return fmt.Errorf("management console: %w", err)
		}
		server, err := startHTTPServer("management console", cfg.Management.Listen, handler)
		if err != nil {
			return err
		}
		servers = append(servers, server)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go pruneLoop(ctx, requestLogs, manager, cfg.Daemon.PruneAt)
	log.Printf("dboss daemon ready: socket=%s proxy=%s management=%s", cfg.Socket, cfg.Proxy.Listen, cfg.Management.Listen)
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		_ = server.Shutdown(shutdown)
	}
	return nil
}

func startHTTPServer(name, address string, handler http.Handler) (*http.Server, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
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

func (c CLI) local(command string, args []string) error {
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	set.SetOutput(c.Err)
	pathFlag := set.String("config", "", "config file")
	jsonOutput := set.Bool("json", false, "JSON output")
	if err := set.Parse(args); err != nil {
		return err
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
			return errors.New("usage: dboss kill [--config path]")
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
	if set.NArg() == 0 {
		var data []byte
		if *jsonOutput {
			data, _ = json.MarshalIndent(cfg, "", "  ")
			data = append(data, '\n')
		} else {
			data, _ = yaml.Marshal(cfg)
		}
		_, err = c.Out.Write(data)
		return err
	}
	if set.NArg() != 1 {
		return errors.New("usage: dboss config [app]")
	}
	found, invalid, err := apps.Discover(cfg)
	if err != nil {
		return err
	}
	for _, appErr := range invalid {
		if strings.HasPrefix(appErr.Error(), set.Arg(0)+":") {
			return appErr
		}
	}
	for _, app := range found {
		if app.Name == set.Arg(0) {
			var data []byte
			if *jsonOutput {
				data, _ = json.MarshalIndent(app.Config, "", "  ")
				data = append(data, '\n')
			} else {
				data, _ = yaml.Marshal(app.Config)
			}
			_, err = c.Out.Write(data)
			return err
		}
	}
	return fmt.Errorf("unknown app %q", set.Arg(0))
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

func (c CLI) remote(command string, args []string, jsonOutput bool, socket string) error {
	client := ctl.Client{Socket: socket}
	request := ctl.Request{Method: command}
	switch command {
	case "ls", "rescan":
		if len(args) != 0 {
			return fmt.Errorf("%s takes no arguments", command)
		}
	case "start", "stop", "restart", "status":
		if len(args) != 1 {
			return fmt.Errorf("usage: dboss %s <app>", command)
		}
		request.App = args[0]
	case "logs":
		if len(args) == 0 {
			return errors.New("usage: dboss logs <app> [-f] [-n 200] [--process name]")
		}
		request.App = args[0]
		set := flag.NewFlagSet("logs", flag.ContinueOnError)
		set.SetOutput(c.Err)
		lines := set.Int("n", 200, "number of lines")
		follow := set.Bool("f", false, "follow")
		processName := set.String("process", "", "process name")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if set.NArg() != 0 {
			return errors.New("usage: dboss logs <app> [-f] [-n 200] [--process name]")
		}
		request.Lines, request.Process = *lines, *processName
		if *follow && jsonOutput {
			return errors.New("--json and -f cannot be combined")
		}
		if *follow {
			return c.follow(client, request)
		}
	case "ports":
		if len(args) != 0 {
			return errors.New("usage: dboss ports")
		}
	default:
		return fmt.Errorf("unknown command %q", command)
	}
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
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%d\n", snapshot.Name, snapshot.State, strings.Join(values, ","), snapshot.Uptime, last, snapshot.Resources.MemoryBytes)
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
	default:
		fmt.Fprintln(c.Out, "ok")
	}
	return nil
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

func commonArgs(args []string) (bool, string, []string, error) {
	jsonOutput, socket := false, ""
	remaining := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOutput = true
		case "--socket":
			if i+1 >= len(args) {
				return false, "", nil, errors.New("--socket requires a path")
			}
			i++
			socket = args[i]
		default:
			remaining = append(remaining, args[i])
		}
	}
	return jsonOutput, socket, remaining, nil
}

func findConfig(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("BOSS_CONFIG"); env != "" {
		return env, nil
	}
	for _, path := range []string{"./deploy-boss.config.yaml", "/etc/boss/deploy-boss.config.yaml"} {
		if _, err := os.Stat(path); err == nil {
			absolute, _ := filepath.Abs(path)
			return absolute, nil
		}
	}
	return "", errors.New("no config found (use --config or BOSS_CONFIG)")
}
