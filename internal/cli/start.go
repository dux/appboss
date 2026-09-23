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

// start runs the session in the foreground. Under systemd this is the service process; on a
// terminal every app's output is echoed with an app/proc prefix, and the apps wait for ENTER
// after the banner so its addresses can be opened first.
func (c CLI) start(args []string) error {
	set := flag.NewFlagSet("start", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := configFlag(set)
	login := set.Bool("login", false, "print a one-time console sign-in link")
	yes := set.Bool("y", false, "start the apps without waiting for ENTER")
	root := set.Bool("root", false, "dev session: listen on :80 and :443 instead of free ports")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: dboss start [-c path] [--login] [-y] [--root]")
	}
	cfg, err := loadHostConfig(*configPath)
	if err != nil {
		return err
	}
	if *root && !cfg.Dev() {
		return errors.New("--root is for a dev session; a host listens on proxy.listen")
	}
	var echo *supervisor.Echo
	if info, statErr := os.Stdout.Stat(); statErr == nil && info.Mode()&os.ModeCharDevice != 0 {
		echo = supervisor.NewEcho(c.Out)
		if cfg.Dev() {
			echo.Solo()
		}
		warnUnignoredRuntime(c.Err, cfg)
		if cfg.Dev() {
			c.offerTrust()
		}
	}
	session, err := daemon.Build(cfg, echo, daemon.Options{Root: *root})
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
	if err := session.Serve(ctx); err != nil {
		return err
	}
	if echo != nil && !*yes && !c.waitForEnter(ctx) {
		return nil
	}
	session.Boot()
	<-ctx.Done()
	return nil
}

// waitForEnter holds the apps until ENTER and reports false when the session is interrupted
// first. Without a terminal on stdin nobody can press it, so it does not wait.
func (c CLI) waitForEnter(ctx context.Context) bool {
	in, ok := c.In.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return true
	}
	fmt.Fprint(c.Out, "Press ENTER to start the apps (dboss start -y skips this) ")
	pressed := make(chan struct{})
	go func() {
		_, _ = bufio.NewReader(in).ReadString('\n')
		close(pressed)
	}()
	select {
	case <-pressed:
		return true
	case <-ctx.Done():
		fmt.Fprintln(c.Out)
		return false
	}
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
