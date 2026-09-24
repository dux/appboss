package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/ctl"
	"dboss/internal/events"
	"dboss/internal/ops"
	"dboss/internal/ports"
	"dboss/internal/supervisor"
)

// check is the pre-deploy gate: the host config and every app must load.
func (c CLI) check(args []string) error {
	cfg, jsonOutput, err := c.hostCommand("check", args)
	if err != nil {
		return err
	}
	_, invalid, err := apps.Discover(cfg)
	if err != nil {
		return err
	}
	if len(invalid) > 0 {
		for _, appErr := range invalid {
			fmt.Fprintln(c.Err, appErr)
		}
		return fmt.Errorf("%d invalid app(s)", len(invalid))
	}
	if jsonOutput {
		fmt.Fprintln(c.Out, `{"ok":true,"invalid":[]}`)
	} else {
		fmt.Fprintln(c.Out, "ok")
	}
	return nil
}

// hostCommand parses a local command that takes no operands and loads the host it runs against.
func (c CLI) hostCommand(name string, args []string) (config.Config, bool, error) {
	set, pathFlag, jsonOutput := c.localFlags(name)
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return config.Config{}, false, err
	}
	if len(operands) != 0 {
		return config.Config{}, false, fmt.Errorf("usage: dboss %s [-c path]", name)
	}
	cfg, err := loadHostConfig(*pathFlag)
	return cfg, *jsonOutput, err
}

// doctor runs the preflight checks a first start or a deploy needs: the tools, the writable
// directories, a valid config and any listeners still holding the app port range.
func (c CLI) doctor(args []string) error {
	cfg, jsonOutput, err := c.hostCommand("doctor", args)
	if err != nil {
		return err
	}
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
	// duckdb is optional: events are stored and counted without it, only SQL and funnels need it.
	if duck, err := events.FindDuckDB(context.Background()); err != nil {
		add("info", err.Error())
	} else {
		add("ok", "duckdb "+duck.Version+" found (event SQL and funnels)")
	}
	for _, dir := range []struct{ name, path string }{{"dir/state", cfg.StateDir}, {"dir/log", cfg.LogDir}, {"dir", filepath.Dir(cfg.Socket)}} {
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
	listeners, listenErr := ports.ListenersInRange(cfg.Ports)
	switch {
	case listenErr != nil:
		add("warn", "port range check failed: "+listenErr.Error())
	case cfg.Dev():
		// The dev sessions of other app folders share the range, so listeners there are expected.
	case len(listeners) > 0:
		pids := make([]int, 0, len(listeners))
		for _, listener := range listeners {
			if len(pids) == 0 || pids[len(pids)-1] != listener.PID {
				pids = append(pids, listener.PID)
			}
		}
		add("warn", fmt.Sprintf("port range %d-%d has listeners (pids %v); a start clears them", cfg.Ports[0], cfg.Ports[1], pids))
	default:
		add("ok", fmt.Sprintf("port range %d-%d is clear", cfg.Ports[0], cfg.Ports[1]))
	}
	// An app binds its own port and dboss only ever dials 127.0.0.1, so a listener on a public
	// address answers without the proxy in front of it. dboss's own listeners are skipped: the
	// console is loopback by design and a hand-run proxy may take a port from the range.
	for _, listener := range listeners {
		if listener.Loopback() || listener.Command == "dboss" {
			continue
		}
		add("warn", fmt.Sprintf("%s (pid %d) listens on %s: that port answers without the proxy, so basic_auth, allow_ips, the sign-in gate and the X-Dboss-User strip do not apply; bind 127.0.0.1 or firewall %d-%d",
			listener.Command, listener.PID, listener.Address, cfg.Ports[0], cfg.Ports[1]))
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

// kill stops every app through the daemon, when one answers, then clears the whole port range.
// A dev session shares the range with other app folders, so it clears only the ports its own
// folder claimed.
func (c CLI) kill(args []string) error {
	cfg, jsonOutput, err := c.hostCommand("kill", args)
	if err != nil {
		return err
	}
	client := ctl.Client{Socket: cfg.Socket}
	var snapshots []supervisor.Snapshot
	err = client.Call(ctl.Request{Method: ops.ActionList}, &snapshots)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("connect to daemon: %w", err)
	}
	stopped := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if err := client.Call(ctl.Request{Method: ops.ActionStop, App: snapshot.Name}, nil); err != nil {
			return fmt.Errorf("stop %s: %w", snapshot.Name, err)
		}
		stopped = append(stopped, snapshot.Name)
	}
	var pids []int
	if cfg.Dev() {
		owned, err := ports.Recorded(filepath.Join(cfg.StateDir, "ports.json"))
		if err != nil {
			return err
		}
		for _, port := range owned {
			killed, err := ports.ClearPort(port, cfg.Defaults.StopTimeout.Value())
			if err != nil {
				return err
			}
			pids = append(pids, killed...)
		}
	} else if pids, err = ports.ClearPortRange(cfg.Ports, cfg.Defaults.StopTimeout.Value()); err != nil {
		return err
	}
	if jsonOutput {
		encoded, _ := json.Marshal(map[string]any{"stopped": stopped, "killed_pids": pids})
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	if cfg.Dev() {
		fmt.Fprintf(c.Out, "stopped %d app(s); killed %d remaining listener(s) on this folder's ports\n", len(stopped), len(pids))
		return nil
	}
	fmt.Fprintf(c.Out, "stopped %d app(s); killed %d remaining listener(s) in ports %d-%d\n", len(stopped), len(pids), cfg.Ports[0], cfg.Ports[1])
	return nil
}
