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

	"dboss/internal/config"
	"dboss/internal/res"
)

const unitPath = "/etc/systemd/system/dboss.service"

// systemd renders the unit that runs `dboss start` for the resolved config, and with --install
// writes it in place and enables it. The unit is generated so the paths always match this box.
func (c CLI) systemd(args []string) error {
	set := flag.NewFlagSet("systemd", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := configFlag(set)
	userName := set.String("user", "", "service user (default: $SUDO_USER under sudo, else the current user)")
	groupName := set.String("group", "", "service group (default: the user's primary group)")
	binary := set.String("bin", "", "dboss binary (default: this executable)")
	install := set.Bool("install", false, "write "+unitPath+", reload systemd and enable the service")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: dboss systemd [-c path] [--user name] [--group name] [--bin path] [--install]")
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
		*userName, err = defaultServiceUser()
		if err != nil {
			return err
		}
	}
	if *userName == "root" {
		fmt.Fprintln(c.Err, "dboss: warning: the unit will run as root; pass --user <name> so an app cannot take the box with it")
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
	home := ""
	if account, lookupErr := user.Lookup(*userName); lookupErr == nil {
		home = account.HomeDir
	}
	unit := renderUnit(cfg, *userName, *groupName, *binary, home)
	if !*install {
		_, err := fmt.Fprint(c.Out, unit)
		return err
	}
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	for _, command := range [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", "dboss"}} {
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Stdout, cmd.Stderr = c.Out, c.Err
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", command, err)
		}
	}
	fmt.Fprintf(c.Out, "installed %s and enabled dboss\n", unitPath)
	return nil
}

func renderUnit(cfg config.Config, userName, groupName, binary, home string) string {
	// Omit Group unless asked: systemd then uses the user's primary group, which need not be
	// named after the user.
	groupLine := ""
	if groupName != "" {
		groupLine = "Group=" + groupName + "\n"
	}
	return fmt.Sprintf(`[Unit]
Description=dboss host %s
After=network.target

[Service]
Type=simple
User=%s
%sWorkingDirectory=%s
Environment=PATH=%s
ExecStartPre=+/bin/sh -c 'mkdir -p %s && chown -R %s %s || true'
ExecStart=%s start -c %s
Restart=always
RestartSec=2
RuntimeDirectory=dboss
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
PrivateTmp=true
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`, cfg.Dir, userName, groupLine, cfg.Dir, servicePATH(home), res.DefaultCgroupRoot, userName, res.DefaultCgroupRoot, quoteUnit(binary), quoteUnit(cfg.SourcePath))
}

// defaultServiceUser is who the unit runs as when --user is not given. Installing writes into
// /etc, so this command is always run through sudo; taking the current user there would name
// root and hand every supervised app the whole box. The invoking account is the useful default,
// the same substitution the Postgres inspector makes.
func defaultServiceUser() (string, error) {
	if os.Geteuid() == 0 {
		if invoker := strings.TrimSpace(os.Getenv("SUDO_USER")); invoker != "" {
			return invoker, nil
		}
	}
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	return current.Username, nil
}

// systemPATH is what systemd hands a unit that sets no PATH of its own.
const systemPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// servicePATH puts the service user's own bin directories first. mise installs itself in
// ~/.local/bin, and `apps.miseEnvironment` shells out to it, so without this the daemon cannot
// find mise and every app with a mise.toml silently runs on the system toolchain.
func servicePATH(home string) string {
	if home == "" {
		return systemPATH
	}
	return home + "/.local/bin:" + home + "/bin:" + systemPATH
}

// quoteUnit wraps a value in systemd's double quotes and escapes the two characters systemd
// treats specially there, so a path with spaces survives. Only Exec* lines support quoting;
// User, Group and WorkingDirectory parse their value literally and must not be quoted.
func quoteUnit(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}
