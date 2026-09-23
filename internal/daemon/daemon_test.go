package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/ports"
)

func TestHostManagementTakesFirstPort(t *testing.T) {
	cfg := config.Default()
	cfg.Ports = [2]int{3100, 3199}
	allocator, registry, err := newAllocator(cfg)
	if err != nil || registry != nil {
		t.Fatalf("host allocator: registry %v, %v", registry, err)
	}
	if port, _ := allocator.Allocate("dboss", "management"); port != 3100 {
		t.Fatalf("management port = %d, entries = %v", port, allocator.Entries())
	}
	if next, _ := allocator.Allocate("alpha", "web"); next != 3101 {
		t.Fatalf("first app port = %d, want 3101", next)
	}
}

func TestManagementAddressUsesLoopback(t *testing.T) {
	if got := managementAddress(3100); got != "127.0.0.1:3100" {
		t.Fatalf("managementAddress = %q", got)
	}
}

func TestStartHTTPServerServesAndRefusesADoubleBind(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	_ = reserved.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	listener, err := bind("test", address, "proxy.listen")
	if err != nil {
		t.Fatal(err)
	}
	server := startHTTPServer("test", listener, handler)
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()

	response, err := http.Get("http://" + address + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}

	if _, err := bind("test", address, "proxy.listen"); err == nil {
		t.Fatal("binding an address already in use should fail")
	}
}

// refuseListen stands in for the kernel refusing a privileged port: a test cannot provoke a real
// EACCES, and it must not depend on whether the test runs as root.
func refuseListen(t *testing.T, refused string) {
	t.Helper()
	original := netListen
	netListen = func(network, address string) (net.Listener, error) {
		if address == refused {
			return nil, &net.OpError{Op: "listen", Net: network, Err: syscall.EACCES}
		}
		return original(network, address)
	}
	t.Cleanup(func() { netListen = original })
}

// freePort borrows a port from the kernel so the fallback range is one the test box really has
// free; 3100 itself is often taken by a dboss the developer is running.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	number, _ := strconv.Atoi(port)
	return number
}

func TestBindProxyFallsBackOnATerminal(t *testing.T) {
	refuseListen(t, ":80")
	port := freePort(t)
	allocator := ports.New([2]int{port, port})

	listener, address, err := bindProxy("proxy", "proxy.listen", ":80", proxyProcess(0), allocator, true)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if want := ":" + strconv.Itoa(port); address != want {
		t.Fatalf("address = %q, want %q", address, want)
	}
	if _, bound, _ := net.SplitHostPort(listener.Addr().String()); bound != strconv.Itoa(port) {
		t.Fatalf("listener on %s, want port %d", listener.Addr(), port)
	}
}

func TestBindProxyKeepsTheHost(t *testing.T) {
	refuseListen(t, "127.0.0.1:80")
	port := freePort(t)
	allocator := ports.New([2]int{port, port})

	listener, address, err := bindProxy("proxy", "proxy.listen", "127.0.0.1:80", proxyProcess(1), allocator, true)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if want := "127.0.0.1:" + strconv.Itoa(port); address != want {
		t.Fatalf("address = %q, want %q", address, want)
	}
	if allocator.Entries()["dboss/proxy-1"] != port {
		t.Fatalf("entries = %v", allocator.Entries())
	}
}

func TestBindProxyStaysFatalWithoutATerminal(t *testing.T) {
	refuseListen(t, ":80")

	_, _, err := bindProxy("proxy", "proxy.listen", ":80", proxyProcess(0), ports.New([2]int{3100, 3990}), false)
	if err == nil {
		t.Fatal("a non-interactive session must not fall back")
	}
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("error = %v, want EACCES", err)
	}
	if !strings.Contains(err.Error(), "proxy.listen") {
		t.Fatalf("error = %v, want the proxy.listen hint", err)
	}
}

func TestLoginURLNeedsTheConsole(t *testing.T) {
	if _, _, err := (&Daemon{}).LoginURL(); err == nil {
		t.Fatal("LoginURL succeeded without a management console")
	}
}

// devSessions points the dev port registry and certificate authority at a temp dir, so a test
// never touches the developer's own sessions, and returns a port window with room to spare.
func devSessions(t *testing.T) [2]int {
	t.Helper()
	dir := t.TempDir()
	previousSessions, previousCA := devSessionsDir, devCADir
	devSessionsDir = func() (string, error) { return filepath.Join(dir, "sessions"), nil }
	devCADir = func() (string, error) { return filepath.Join(dir, "ca"), nil }
	t.Cleanup(func() { devSessionsDir, devCADir = previousSessions, previousCA })
	first := freePort(t)
	return [2]int{first, first + 50}
}

// devConfig is a dev session for an app folder with no processes.
func devConfig(t *testing.T, window [2]int) config.Config {
	t.Helper()
	// Not t.TempDir(): its path plus the socket name overruns the unix socket length limit.
	dir, err := os.MkdirTemp("", "dboss")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg := config.Default()
	cfg.Dir = dir
	cfg.SourcePath = dir + "/" + config.FileName
	if err := os.WriteFile(cfg.SourcePath, []byte("procfile: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.StateDir, cfg.LogDir, cfg.Socket = dir+"/state", dir+"/log", dir+"/dboss.sock"
	cfg.Ports = window
	cfg.App = &config.App{Procfile: map[string]config.ProcessSpec{}}
	return cfg
}

// A dev session gets its console, `dboss login` and a proxy on claimed ports, and hands the
// console port to the config so hook links point at it.
func TestDevSessionServesOnClaimedPorts(t *testing.T) {
	cfg := devConfig(t, devSessions(t))
	session, err := Build(cfg, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if session.management == nil {
		t.Fatal("a dev session should have a console")
	}
	if _, _, err := session.LoginURL(); err != nil {
		t.Fatalf("LoginURL = %v", err)
	}
	if session.managementPort < cfg.Ports[0] || session.managementPort > cfg.Ports[1] {
		t.Fatalf("console on %d, outside %v", session.managementPort, cfg.Ports)
	}
	if len(session.listen) != 1 || session.listen[0] == ":80" {
		t.Fatalf("proxy listen = %v, want a claimed port", session.listen)
	}
	if got := session.manager.HostConfig().ConsoleURL(); got != "http://127.0.0.1:"+strconv.Itoa(session.managementPort) {
		t.Fatalf("ConsoleURL = %q", got)
	}
}

// Two dev sessions in different app folders run side by side: neither clears the other's
// listeners and no port is handed out twice.
func TestDevSessionsRunSideBySide(t *testing.T) {
	window := devSessions(t)
	first, err := Build(devConfig(t, window), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Build(devConfig(t, window), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.managementPort == second.managementPort || first.listen[0] == second.listen[0] {
		t.Fatalf("sessions share a port: console %d/%d, proxy %s/%s", first.managementPort, second.managementPort, first.listen[0], second.listen[0])
	}
	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(first.managementPort) + "/healthz")
	if err != nil {
		t.Fatalf("first console gone after the second start: %v", err)
	}
	_ = response.Body.Close()
}

// --root asks for :80 and :443 exactly, so a refused bind is an error that names the flag instead
// of a quiet move to a free port.
func TestDevRootBindsStandardPorts(t *testing.T) {
	cfg := devConfig(t, devSessions(t))
	refuseListen(t, ":80")
	_, err := Build(cfg, nil, Options{Root: true})
	if err == nil || !strings.Contains(err.Error(), "--root") || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("error = %v, want the --root hint", err)
	}
}

// A dev session serves the proxy over HTTPS with a leaf from the local authority, so a client
// that trusts the root completes the handshake for any app host.
func TestDevSessionServesHTTPS(t *testing.T) {
	cfg := devConfig(t, devSessions(t))
	session, err := Build(cfg, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	address := session.devHTTPS
	if address == "" || session.devTLS == nil {
		t.Fatal("dev session has no https listener")
	}
	rootPEM, err := os.ReadFile(session.devTLS.RootPath())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(rootPEM)
	conn, err := tls.Dial("tcp", address, &tls.Config{RootCAs: roots, ServerName: "demo.lvh.me"})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = conn.Close()
}
