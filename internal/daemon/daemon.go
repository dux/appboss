// Package daemon wires one host session: the supervisor, the module set, the proxy pipeline,
// the management console and the control socket. cli.start only loads the config and calls
// Build and Run, so a new module is registered here and nowhere else.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"deploy-boss/internal/apps"
	"deploy-boss/internal/config"
	"deploy-boss/internal/console"
	"deploy-boss/internal/ctl"
	"deploy-boss/internal/ingest"
	"deploy-boss/internal/logstore"
	"deploy-boss/internal/module"
	"deploy-boss/internal/ops"
	"deploy-boss/internal/ports"
	"deploy-boss/internal/proxy"
	"deploy-boss/internal/super"
)

// Daemon is one running host session. New builds and binds it; Run serves until the context is
// cancelled, then Close drains it.
type Daemon struct {
	cfg            config.Config
	manager        *super.Manager
	modules        *module.Manager
	control        *ctl.Server
	management     *console.Handler
	servers        []*http.Server
	managementPort int
}

// Build prepares the session: directories, port range, supervisor, modules, the proxy pipeline,
// the console and the control socket. Listeners bind here, so a returned error leaves nothing
// behind.
func Build(cfg config.Config, echo *super.Echo) (*Daemon, error) {
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
		log.Printf("cleared app port range %d-%d: pids=%v", cfg.Ports.Range[0], cfg.Ports.Range[1], cleared)
	}
	allocator, managementPort := newAllocator(cfg)
	manager, invalid, err := super.New(cfg, allocator, echo)
	if err != nil {
		return nil, err
	}
	for _, scanErr := range invalid {
		log.Printf("skip invalid app: %v", scanErr)
	}
	logs := logstore.New(cfg.LogDir, cfg.Defaults.LogFlush.Value(), manager, cfg.Daemon.PruneAt, cfg.Defaults.StdoutRetention.Value())
	if retention := cfg.Defaults.StdoutRetention.Value(); retention > 0 {
		log.SetOutput(io.MultiWriter(log.Writer(), ingest.NewDaemonSink(logs)))
	}
	ingester := ingest.New(manager, manager, logs, cfg.Daemon.LogIngestInterval.Value())
	d := &Daemon{cfg: cfg, manager: manager, modules: module.NewManager(logs, ingester), managementPort: managementPort}
	service := ops.New(manager, logs, logs)
	if len(cfg.Proxy.Listen) > 0 {
		edge, management, err := edgeHandler(cfg, service, manager, logs)
		if err != nil {
			d.Close()
			return nil, err
		}
		d.management = management
		for _, address := range cfg.Proxy.Listen {
			server, err := startHTTPServer("proxy", address, edge)
			if err != nil {
				d.Close()
				return nil, err
			}
			d.servers = append(d.servers, server)
		}
		if management != nil {
			server, err := startHTTPServer("management", managementAddress(managementPort), management)
			if err != nil {
				d.Close()
				return nil, err
			}
			d.servers = append(d.servers, server)
		}
	}
	var login func() (string, error)
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
		log.Printf("management console: http://127.0.0.1:%d (run `dboss login` for a one-time sign-in link)", d.managementPort)
		if d.cfg.Management.URL != "" {
			log.Printf("management console: %s (AuthCog sign-in)", d.cfg.Management.URL)
		}
	}
	log.Printf("dboss ready: config=%s socket=%s listen=%s management=%s port=%d", d.cfg.SourcePath, d.cfg.Socket, strings.Join(d.cfg.Proxy.Listen, ","), strings.Join(d.cfg.Management.Host, ","), d.managementPort)
	<-ctx.Done()
	return nil
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
func edgeHandler(cfg config.Config, service *ops.Service, manager *super.Manager, logs proxy.Recorder) (http.Handler, *console.Handler, error) {
	appProxy, err := proxy.New(cfg, manager, logs)
	if err != nil {
		return nil, nil, err
	}
	var handler http.Handler = appProxy
	var management *console.Handler
	if cfg.Management.Enabled() {
		management, err = console.New(cfg, service, apps.NewStore(cfg))
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
