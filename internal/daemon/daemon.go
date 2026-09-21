// Package daemon wires one host session: the supervisor, the module set, the proxy pipeline,
// the management console and the control socket. cli.start only loads the config and calls
// Build and Run, so a new module is registered here and nowhere else.
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
	"dboss/internal/config"
	"dboss/internal/console"
	"dboss/internal/ctl"
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
	"dboss/internal/super"
	"dboss/internal/sysinfo"
)

// Daemon is one running host session. New builds and binds it; Run serves until the context is
// cancelled, then Close drains it.
type Daemon struct {
	cfg            config.Config
	manager        *super.Manager
	modules        *module.Manager
	control        *ctl.Server
	management     *console.Handler
	notifier       *notify.Notifier
	servers        []*http.Server
	listen         []string
	echo           *super.Echo
	managementPort int
}

// netListen is the test seam for the privileged-port fallback: a test cannot provoke a real
// EACCES portably.
var netListen = net.Listen

// Build prepares the session: directories, port range, supervisor, modules, the proxy pipeline,
// the console and the control socket. Listeners bind here, so a returned error leaves nothing
// behind.
func Build(cfg config.Config, echo *super.Echo) (*Daemon, error) {
	logx.SetLevel(cfg.Daemon.LogLevel)
	for _, dir := range []string{cfg.StateDir, cfg.LogDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	cleared, err := super.ClearPortRange(cfg.Ports.Range, cfg.Defaults.StopTimeout.Value())
	if err != nil {
		return nil, err
	}
	if len(cleared) > 0 {
		logx.Infof("cleared app port range %d-%d: pids=%v", cfg.Ports.Range[0], cfg.Ports.Range[1], cleared)
	}
	allocator, managementPort := newAllocator(cfg)
	notifier := notify.New(notify.Config{URL: cfg.Notify.URL, Format: cfg.Notify.Format, Events: cfg.Notify.Events, MinInterval: cfg.Notify.MinInterval.Value(), Headers: cfg.Notify.Headers})
	// The supervisor resolves an app's pg_db block through this service before it spawns, so it
	// is built ahead of the manager. It is still the one value registered as a module below.
	postgres := pg.New(cfg, notifier)
	manager, invalid, err := super.New(cfg, allocator, echo, postgres, notifier)
	if err != nil {
		notifier.Close()
		return nil, err
	}
	for _, scanErr := range invalid {
		logx.Warnf("skip invalid app: %v", scanErr)
	}
	logs := logstore.New(cfg.LogDir, cfg.Defaults.LogFlush.Value(), manager, cfg.Daemon.PruneAt, cfg.Daemon.VacuumAt, cfg.Defaults.StdoutRetention.Value(), cfg.Daemon.AuditRetention.Value())
	if retention := cfg.Defaults.StdoutRetention.Value(); retention > 0 {
		log.SetOutput(io.MultiWriter(log.Writer(), ingest.NewDaemonSink(logs)))
	}
	ingester := ingest.New(manager, manager, logs, cfg.Daemon.LogIngestInterval.Value())
	sysInfo := sysinfo.New([]sysinfo.DirSpec{
		{Name: "config", Path: cfg.Dir},
		{Name: "state_dir", Path: cfg.StateDir},
		{Name: "log_dir", Path: cfg.LogDir},
		{Name: "socket", Path: filepath.Dir(cfg.Socket)},
	})
	channels, err := pubsub.New(cfg.StateDir)
	if err != nil {
		notifier.Close()
		manager.Close()
		return nil, err
	}
	d := &Daemon{cfg: cfg, manager: manager, modules: module.NewManager(logs, ingester, alerts.New(manager, logs, notifier), sysInfo, postgres, channels), notifier: notifier, echo: echo, managementPort: managementPort}
	service := ops.New(manager, logs, logs, postgres, channels, notifier)
	if len(cfg.Proxy.Listen) > 0 {
		edge, management, err := edgeHandler(cfg, service, manager, logs, notifier, sysInfo.Inspector(), channels)
		if err != nil {
			d.Close()
			return nil, err
		}
		d.management = management
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
			if certs != nil && cfg.Proxy.TLS.Redirect {
				handler = certs.HTTPHandler(nil)
			}
			listener, bound, err := bindProxy(address, proxyProcess(index), allocator, echo != nil)
			if err != nil {
				d.Close()
				return nil, err
			}
			d.servers = append(d.servers, startHTTPServer("proxy", listener, handler))
			d.listen = append(d.listen, bound)
		}
		if management != nil {
			listener, err := bind("management", managementAddress(managementPort), "ports.range")
			if err != nil {
				d.Close()
				return nil, err
			}
			d.servers = append(d.servers, startHTTPServer("management", listener, management))
		}
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

// Run starts the modules and serves until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.modules.Start(ctx); err != nil {
		return err
	}
	if d.management != nil {
		logx.Infof("management console: http://127.0.0.1:%d (run `dboss login` for a one-time sign-in link)", d.managementPort)
		if publicURL := d.cfg.Management.PublicURL(); publicURL != "" {
			logx.Infof("management console: %s (AuthCog sign-in)", publicURL)
		}
	}
	d.printBanner()
	logx.Infof("dboss ready: config=%s socket=%s listen=%s management=%s port=%d", d.cfg.SourcePath, d.cfg.Socket, strings.Join(d.listen, ","), strings.Join(d.cfg.Management.Host, ","), d.managementPort)
	<-ctx.Done()
	return nil
}

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
func edgeHandler(cfg config.Config, service *ops.Service, manager *super.Manager, logs proxy.Recorder, notifier *notify.Notifier, sys console.SysReader, channels *pubsub.Service) (http.Handler, *console.Handler, error) {
	appProxy, err := proxy.New(cfg, manager, logs, channels, channels.Filter)
	if err != nil {
		return nil, nil, err
	}
	var handler http.Handler = appProxy
	var management *console.Handler
	if cfg.Management.Enabled() {
		management, err = console.New(cfg, service, apps.NewStore(cfg), notifier.Stats, sys)
		if err != nil {
			return nil, nil, fmt.Errorf("management console: %w", err)
		}
		handler = proxy.HostSwitch(cfg.Management.Host, management, appProxy)
	}
	edge, err := proxy.TrustedOnly(cfg.Proxy.TrustedCIDRs, handler)
	if err != nil {
		return nil, nil, err
	}
	return proxy.CloudflareOnly(cfg.Proxy.CloudflareOnly, edge), management, nil
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

// bindProxy is bind for a plain proxy address, with the terminal fallback: a hand-run session
// that may not hold the configured port moves to the first free port of ports.range instead of
// exiting, so developing against an app needs no sudo. Under systemd stdout is a pipe, so a
// service start still fails.
func bindProxy(address, process string, allocator *ports.Allocator, interactive bool) (net.Listener, string, error) {
	listener, err := netListen("tcp", address)
	if err == nil {
		return listener, address, nil
	}
	if !interactive || !errors.Is(err, syscall.EACCES) {
		return nil, "", bindError("proxy", address, "proxy.listen", err)
	}
	port, allocErr := allocator.Allocate("dboss", process)
	if allocErr != nil {
		return nil, "", bindError("proxy", address, "proxy.listen", err)
	}
	host, _, splitErr := net.SplitHostPort(address)
	if splitErr != nil {
		return nil, "", bindError("proxy", address, "proxy.listen", err)
	}
	fallback := net.JoinHostPort(host, strconv.Itoa(port))
	listener, err = netListen("tcp", fallback)
	if err != nil {
		return nil, "", fmt.Errorf("proxy listen %s: %w", fallback, err)
	}
	logx.Warnf("proxy: %s needs root or CAP_NET_BIND_SERVICE; this terminal session listens on %s instead", address, fallback)
	return listener, fallback, nil
}

// proxyProcess is the allocator key for the nth proxy.listen entry, so several addresses that
// all fall back get one port each.
func proxyProcess(index int) string {
	if index == 0 {
		return "proxy"
	}
	return "proxy-" + strconv.Itoa(index)
}
