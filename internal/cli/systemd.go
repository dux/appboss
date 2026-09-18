package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"app-boss/internal/config"
)

const unitPath = "/etc/systemd/system/appboss.service"

// systemd renders the unit that runs `appboss start` for the resolved config, and with --install
// writes it in place and enables it. The unit is generated so the paths always match this box.
func (c CLI) systemd(args []string) error {
	set := flag.NewFlagSet("systemd", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := configFlag(set)
	userName := set.String("user", "", "service user (default: current user)")
	groupName := set.String("group", "", "service group (default: the user's primary group)")
	binary := set.String("bin", "", "appboss binary (default: this executable)")
	install := set.Bool("install", false, "write "+unitPath+", reload systemd and enable the service")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: appboss systemd [-c path] [--user name] [--group name] [--bin path] [--install]")
	}
	path, err := findConfig(*configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if *userName == "" {
		current, err := user.Current()
		if err != nil {
			return err
		}
		*userName = current.Username
	}
	if *binary == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		if *binary, err = filepath.EvalSymlinks(executable); err != nil {
			return err
		}
	}
	unit := renderUnit(cfg, *userName, *groupName, *binary)
	if !*install {
		_, err := fmt.Fprint(c.Out, unit)
		return err
	}
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	for _, command := range [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", "appboss"}} {
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Stdout, cmd.Stderr = c.Out, c.Err
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", command, err)
		}
	}
	fmt.Fprintf(c.Out, "installed %s and enabled appboss\n", unitPath)
	return nil
}

func renderUnit(cfg config.Config, userName, groupName, binary string) string {
	// Omit Group unless asked: systemd then uses the user's primary group, which need not be
	// named after the user.
	groupLine := ""
	if groupName != "" {
		groupLine = "Group=" + quoteUnit(groupName) + "\n"
	}
	return fmt.Sprintf(`[Unit]
Description=appboss host %s
After=network.target

[Service]
Type=simple
User=%s
%sWorkingDirectory=%s
ExecStart=%s start -c %s
Restart=always
RestartSec=2
RuntimeDirectory=appboss
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
PrivateTmp=true
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`, cfg.Dir, quoteUnit(userName), groupLine, quoteUnit(cfg.Dir), quoteUnit(binary), quoteUnit(cfg.SourcePath))
}

// quoteUnit wraps a value in systemd's double quotes and escapes the two characters systemd
// treats specially there, so a path with spaces survives.
func quoteUnit(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}
