package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"dboss/internal/daemon"
	"dboss/internal/devtls"
	"dboss/internal/supervisor"

	"golang.org/x/term"
)

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
	var echo *supervisor.Echo
	if info, statErr := os.Stdout.Stat(); statErr == nil && info.Mode()&os.ModeCharDevice != 0 {
		echo = supervisor.NewEcho(c.Out)
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
