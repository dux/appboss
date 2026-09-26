// Package daemon wires one host session: the supervisor, the module set, the proxy pipeline,
// the management console and the control socket. cli.start only loads the config and calls
// Build, Serve and Boot, so a new module is registered here and nowhere else.
package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dboss/internal/alerts"
	"dboss/internal/apps"
	"dboss/internal/authcog"
	"dboss/internal/config"
	"dboss/internal/console"
	"dboss/internal/ctl"
	"dboss/internal/devtls"
	"dboss/internal/diskusage"
	"dboss/internal/events"
	"dboss/internal/ingest"
	"dboss/internal/logstore"
	"dboss/internal/logx"
	"dboss/internal/module"
	"dboss/internal/notify"
	"dboss/internal/ops"
	"dboss/internal/pg"
	"dboss/internal/ports"
	"dboss/internal/proxy"
	"dboss/internal/pubsub"
	"dboss/internal/supervisor"
	"dboss/internal/sysinfo"
	"dboss/internal/tmpclean"
)

// Daemon is one running session. Build binds it, Serve starts the modules, Boot starts the apps,
// and Close drains it.
type Daemon struct {
	cfg            config.Config
	manager        *supervisor.Manager
	modules        *module.Manager
	control        *ctl.Server
	management     *console.Handler
	notifier       *notify.Notifier
	servers        []*http.Server
	listen         []string
	devHTTPS       string // actual address of the dev session's HTTPS listener
	devTLS         *devtls.Authority
	echo           *supervisor.Echo
	managementPort int
	registry       *ports.Registry // a dev session's claim on its ports
}

// Options are the start flags of a hand-run session.
type Options struct {
	// HTTPS binds a dev session's proxy on :80 and adds HTTPS on :443 with certificates from the
	// local authority, instead of plain http on a free port.
	HTTPS bool
}

// netListen is the test seam for the privileged-port fallback: a test cannot provoke a real
// EACCES portably.
var netListen = net.Listen

// Built-in cadence: the log store writes queued rows every logFlush, process logs are sealed and
// ingested every logIngestInterval, and one app event is notified at most once per notifyQuiet.
const (
	logFlush          = time.Second
	logIngestInterval = 5 * time.Second
	notifyQuiet       = 5 * time.Minute
)

// devCADir and devSessionsDir are the test seams for the per-user dev state: the certificate
// authority and the registry every dev session claims its ports from.
var (
	devCADir       = devtls.DefaultDir
	devSessionsDir = ports.DefaultSessionsDir
)

// Build prepares the session: directories, ports, supervisor, modules, the proxy pipeline, the
// console and the control socket. Listeners bind here, so a returned error leaves nothing behind.
// No app starts before Boot.
func Build(cfg config.Config, echo *supervisor.Echo, opts Options) (*Daemon, error) {
	logx.SetLevel(cfg.LogLevel)
	for _, dir := range []string{cfg.StateDir, cfg.LogDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	if !cfg.Dev() {
		cleared, err := ports.ClearPortRange(cfg.Ports, cfg.Defaults.StopTimeout.Value())
		if err != nil {
			return nil, err
		}
		if len(cleared) > 0 {
			logx.Infof("cleared app port range %d-%d: pids=%v", cfg.Ports[0], cfg.Ports[1], cleared)
		}
	}
	allocator, registry, err := newAllocator(cfg)
	if err != nil {
		return nil, err
	}
	managementPort, err := allocator.Allocate("dboss", "management")
	if err != nil {
		closeRegistry(registry)
		return nil, err
	}
	cfg.ConsolePort = managementPort
	// A hand-run terminal session admits its own loopback without a token; systemd has no terminal.
	cfg.Local = echo != nil
	notifier := notify.New(notify.Config{URL: cfg.Notify.URL, Format: notify.FormatFor(cfg.Notify.URL), Events: cfg.Notify.Events, MinInterval: notifyQuiet, Headers: cfg.Notify.Headers})
	manager, invalid, err := supervisor.New(cfg, allocator, echo, notifier)
	if err != nil {
		notifier.Close()
		closeRegistry(registry)
		return nil, err
	}
	for _, scanErr := range invalid {
		logx.Warnf("skip invalid app: %v", scanErr)
	}
	logs := logstore.New(cfg.LogDir, logFlush, manager, cfg.MaintenanceAt, cfg.Defaults.StdoutRetention.Value(), cfg.AuditRetention.Value())
	if retention := cfg.Defaults.StdoutRetention.Value(); retention > 0 {
		log.SetOutput(io.MultiWriter(log.Writer(), ingest.NewDaemonSink(logs)))
	}
	eventStore := events.NewStore(cfg.LogDir)
	eventService := events.NewService(eventStore, events.NewSavedStore(cfg.StateDir), eventApps{manager})
	eventModule := events.NewModule(eventStore, eventService.Saved, eventService.Apps, cfg.MaintenanceAt, cfg.Defaults.Events.Retention.Value())
	ingester := ingest.New(manager, manager, logs, eventStore, logIngestInterval)
	sysInfo := sysinfo.New([]sysinfo.DirSpec{
		{Name: "config", Path: cfg.Dir},
		{Name: "apps", Path: cfg.Apps},
		{Name: "dir/state", Path: cfg.StateDir},
		{Name: "dir/log", Path: cfg.LogDir},
		{Name: "dir", Path: cfg.RuntimeDir},
	})
	channels, err := pubsub.New(cfg.StateDir)
	if err != nil {
		notifier.Close()
		manager.Close()
		closeRegistry(registry)
		return nil, err
	}
	sizes := diskusage.New(manager, cfg.LogDir)
	postgres := pg.New(cfg, notifier)
	d := &Daemon{cfg: cfg, manager: manager, modules: module.NewManager(logs, ingester, eventModule, alerts.New(manager, logs, sysInfo.Inspector(), notifier), tmpclean.New(manager), sizes, sysInfo, postgres, channels), notifier: notifier, echo: echo, managementPort: managementPort, registry: registry}
	service := ops.New(manager, logs, postgres, channels, sizes, notifier)
	service.SetEvents(eventService)
	// One AuthCog flow for the console and every app gate: one signing key, one challenge map.
	flow, err := authcog.New(cfg.StateDir)
	if err != nil {
		d.Close()
		return nil, err
	}
	// The console has its own loopback listener, so it is built and bound outside the proxy
	// block: a dev session with proxy.listen turned off still gets a console and `dboss login`.
	var management *console.Handler
	if cfg.ConsoleEnabled() {
		management, err = console.New(cfg, flow, service, apps.NewStore(cfg), sysInfo.Inspector())
		if err != nil {
			d.Close()
			return nil, fmt.Errorf("management console: %w", err)
		}
		d.management = management
	}
	if cfg.Dev() {
		edge, err := edgeHandler(cfg, flow, manager, logs, management, channels)
		if err != nil {
			d.Close()
			return nil, err
		}
		if err := d.startDevProxy(edge, allocator, opts.HTTPS); err != nil {
			d.Close()
			return nil, err
		}
	} else if len(cfg.Proxy.Listen) > 0 {
		edge, err := edgeHandler(cfg, flow, manager, logs, management, channels)
		if err != nil {
			d.Close()
			return nil, err
		}
		var certs *proxy.ACME
		if cfg.Proxy.TLS.Enabled() {
			certs, err = proxy.NewACME(cfg, manager)
			if err != nil {
				d.Close()
				return nil, err
			}
			listener, err := bind("proxy-tls", cfg.Proxy.TLS.Listen, "proxy.tls.listen")
			if err != nil {
				d.Close()
				return nil, err
			}
			d.servers = append(d.servers, startHTTPSServer("proxy-tls", listener, edge, certs.TLSConfig()))
			logx.Infof("proxy tls: %s (acme on demand)", cfg.Proxy.TLS.Listen)
		}
		for index, address := range cfg.Proxy.Listen {
			handler := http.Handler(edge)
			if certs != nil {
				handler = certs.HTTPHandler(edge)
			}
			listener, bound, err := bindProxy("proxy", "proxy.listen", address, proxyProcess(index), allocator, echo != nil)
			if err != nil {
				d.Close()
				return nil, err
			}
			d.servers = append(d.servers, startHTTPServer("proxy", listener, handler))
			d.listen = append(d.listen, bound)
		}
	}
	if management != nil {
		management.SetAppAddress(d.appAddress())
		listener, err := bind("management", managementAddress(managementPort), "ports")
		if err != nil {
			d.Close()
			return nil, err
		}
		d.servers = append(d.servers, startHTTPServer("management", listener, management))
	}
	var login func() (string, string, error)
	if d.management != nil {
		login = d.management.LoginURL
	}
	control, err := ctl.Listen(cfg.Socket, service, login)
	if err != nil {
		d.Close()
		return nil, err
	}
	d.control = control
	return d, nil
}

// Serve starts the modules and prints the banner. Every listener already answers, but the apps
// wait for Boot, so a hand-run start can show its addresses before anything loads.
func (d *Daemon) Serve(ctx context.Context) error {
	if err := d.modules.Start(ctx); err != nil {
		return err
	}
	if d.management != nil {
		if d.cfg.Dev() || d.cfg.Local {
			logx.Infof("management console: http://127.0.0.1:%d (open from this machine, no sign-in)", d.managementPort)
		} else {
			logx.Infof("management console: http://127.0.0.1:%d (run `dboss login` for a one-time sign-in link)", d.managementPort)
		}
		if publicURL := d.cfg.Management.PublicURL(); publicURL != "" {
			logx.Infof("management console: %s (AuthCog sign-in)", publicURL)
		}
	}
	logx.Infof("dboss ready: config=%s socket=%s listen=%s management=%s port=%d", d.cfg.SourcePath, d.cfg.Socket, strings.Join(d.listen, ","), strings.Join(d.cfg.Management.Host, ","), d.managementPort)
	// Last, so the banner sits right above the ENTER prompt.
	d.printBanner()
	return nil
}

// Boot starts the apps that were running before.
func (d *Daemon) Boot() { d.manager.Boot() }

// LoginURL mints a one-time console sign-in link; it fails when the console is off.
func (d *Daemon) LoginURL() (local, public string, err error) {
	if d.management == nil {
		return "", "", errors.New("management console is not configured")
	}
	return d.management.LoginURL()
}

// Close drains the listeners, modules and supervisor. It is safe to call more than once and on
// a half-built daemon.
func (d *Daemon) Close() error {
	for _, server := range d.servers {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = server.Shutdown(shutdown)
		cancel()
	}
	if d.control != nil {
		_ = d.control.Close()
	}
	_ = d.modules.Close()
	d.manager.Close()
	if d.notifier != nil {
		d.notifier.Close()
	}
	closeRegistry(d.registry)
	return nil
}

// newAllocator picks how the session gets its ports. A host owns its whole range, cleared by
// Build, and counts up from the start, so the console always lands on the first port. A dev
// session shares the range with the dev sessions of other app folders, so it claims every port
// from the machine-wide registry and only clears what an earlier run of its own folder left.
func newAllocator(cfg config.Config) (*ports.Allocator, *ports.Registry, error) {
	if !cfg.Dev() {
		return ports.New(cfg.Ports), nil, nil
	}
	dir, err := devSessionsDir()
	if err != nil {
		return nil, nil, err
	}
	registry, err := ports.OpenRegistry(dir, filepath.Join(cfg.StateDir, "ports.json"), cfg.Ports)
	if err != nil {
		return nil, nil, err
	}
	stale, err := registry.Stale()
	if err != nil {
		closeRegistry(registry)
		return nil, nil, err
	}
	for _, port := range stale {
		cleared, err := ports.ClearPort(port, cfg.Defaults.StopTimeout.Value())
		if err != nil {
			closeRegistry(registry)
			return nil, nil, err
		}
		if len(cleared) > 0 {
			logx.Infof("cleared port %d left by an earlier run: pids=%v", port, cleared)
		}
	}
	return ports.NewClaiming(registry.Claim), registry, nil
}

func closeRegistry(registry *ports.Registry) {
	if registry != nil {
		_ = registry.Close()
	}
}

func managementAddress(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

// edgeHandler is the single public listener: Cloudflare hands it the full request and the
// host header picks the console or an app. Only the app proxy is affected by the trusted CIDRs.
// A console with no management.host is left off the switch; it is reached on its own port.
func edgeHandler(cfg config.Config, flow *authcog.Flow, manager *supervisor.Manager, logs proxy.Recorder, management *console.Handler, channels *pubsub.Service) (http.Handler, error) {
	appProxy, err := proxy.New(cfg, flow, manager, logs, channels, channels.Filter)
	if err != nil {
		return nil, err
	}
	var handler http.Handler = appProxy
	if management != nil && cfg.Management.Enabled() {
		handler = proxy.HostSwitch(cfg.Management.Host, management, appProxy)
	}
	return proxy.CloudflareOnly(cfg.Proxy.Cloudflare, handler), nil
}

func startHTTPServer(name string, listener net.Listener, handler http.Handler) *http.Server {
	server := &http.Server{Addr: listener.Addr().String(), Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logx.Errorf("%s: %v", name, err)
		}
	}()
	return server
}

// startHTTPSServer is startHTTPServer with TLS. The certificate comes from the config's
// GetCertificate, so autocert can issue and renew it without a file or a reload.
func startHTTPSServer(name string, listener net.Listener, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	server := &http.Server{Addr: listener.Addr().String(), Handler: handler, ReadHeaderTimeout: 10 * time.Second, TLSConfig: tlsConfig}
	go func() {
		if err := server.ServeTLS(listener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logx.Errorf("%s: %v", name, err)
		}
	}()
	return server
}

// bind opens a listener, turning a privileged-port refusal into an actionable error; key names
// the config key that moves the listener off the port.
func bind(name, address, key string) (net.Listener, error) {
	listener, err := netListen("tcp", address)
	if err != nil {
		return nil, bindError(name, address, key, err)
	}
	return listener, nil
}

func bindError(name, address, key string, err error) error {
	if errors.Is(err, syscall.EACCES) {
		return fmt.Errorf("%s listen %s: %w (a port below 1024 needs root or CAP_NET_BIND_SERVICE: install the service with `dboss systemd --install`, start it with sudo, or set %s to a port above 1023)", name, address, err, key)
	}
	return fmt.Errorf("%s listen %s: %w", name, address, err)
}

// bindProxy is bind for a proxy address, with the terminal fallback: a hand-run session that may
// not hold the configured port moves to the first free port of ports instead of exiting,
// so developing against an app needs no sudo. Under systemd stdout is a pipe, so a service start
// still fails. name and key label the listener and the config key that moves it in errors.
func bindProxy(name, key, address, process string, allocator *ports.Allocator, interactive bool) (net.Listener, string, error) {
	listener, err := netListen("tcp", address)
	if err == nil {
		return listener, address, nil
	}
	if !interactive || !errors.Is(err, syscall.EACCES) {
		return nil, "", bindError(name, address, key, err)
	}
	port, allocErr := allocator.Allocate("dboss", process)
	if allocErr != nil {
		return nil, "", bindError(name, address, key, err)
	}
	host, _, splitErr := net.SplitHostPort(address)
	if splitErr != nil {
		return nil, "", bindError(name, address, key, err)
	}
	fallback := net.JoinHostPort(host, strconv.Itoa(port))
	listener, err = netListen("tcp", fallback)
	if err != nil {
		return nil, "", fmt.Errorf("%s listen %s: %w", name, fallback, err)
	}
	logx.Warnf("%s: %s needs root or CAP_NET_BIND_SERVICE; this terminal session listens on %s instead", name, address, fallback)
	return listener, fallback, nil
}

// startDevProxy serves a dev session's plain http on the next free port, or with --https on :80
// plus HTTPS on :443. Both addresses are asked for exactly, so a failed bind is an error.
func (d *Daemon) startDevProxy(edge http.Handler, allocator *ports.Allocator, https bool) error {
	address, err := devAddress(allocator, "proxy", ":80", https)
	if err != nil {
		return err
	}
	listener, err := devBind("proxy", address, https)
	if err != nil {
		return err
	}
	d.servers = append(d.servers, startHTTPServer("proxy", listener, edge))
	d.listen = append(d.listen, address)
	if !https {
		return nil
	}
	dir, err := devCADir()
	if err == nil {
		d.devTLS, err = devtls.Open(dir)
	}
	if err != nil {
		return fmt.Errorf("dev https: %w", err)
	}
	listener, err = devBind("dev https", ":443", https)
	if err != nil {
		d.devTLS = nil
		return err
	}
	d.servers = append(d.servers, startHTTPSServer("dev-https", listener, edge, d.devTLS.TLSConfig()))
	d.devHTTPS = listener.Addr().String()
	logx.Infof("dev https: %s (local certificate authority %s)", d.devHTTPS, d.devTLS.RootPath())
	return nil
}

// devAddress is where a dev listener goes: the fixed port under --https, else a claimed one.
func devAddress(allocator *ports.Allocator, process, fixed string, https bool) (string, error) {
	if https {
		return fixed, nil
	}
	port, err := allocator.Allocate("dboss", process)
	if err != nil {
		return "", fmt.Errorf("%s: %w", process, err)
	}
	return ":" + strconv.Itoa(port), nil
}

func devBind(name, address string, https bool) (net.Listener, error) {
	listener, err := netListen("tcp", address)
	if err == nil {
		return listener, nil
	}
	if https {
		return nil, fmt.Errorf("%s listen %s: %w (dboss start --https needs %s free and, on Linux, root or CAP_NET_BIND_SERVICE; start without --https to use a free port)", name, address, err, address)
	}
	return nil, fmt.Errorf("%s listen %s: %w", name, address, err)
}

// proxyProcess is the allocator key for the nth proxy.listen entry, so several addresses that
// all fall back get one port each.
func proxyProcess(index int) string {
	if index == 0 {
		return "proxy"
	}
	return "proxy-" + strconv.Itoa(index)
}
