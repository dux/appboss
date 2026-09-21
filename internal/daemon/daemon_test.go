package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/ports"
)

func TestManagementTakesFirstPort(t *testing.T) {
	cfg := config.Default()
	cfg.Ports.Range = [2]int{3100, 3199}
	allocator, port := newAllocator(cfg)
	if port != 3100 || allocator.Entries()["dboss/management"] != 3100 {
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

	listener, address, err := bindProxy(":80", proxyProcess(0), allocator, true)
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

	listener, address, err := bindProxy("127.0.0.1:80", proxyProcess(1), allocator, true)
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

	_, _, err := bindProxy(":80", proxyProcess(0), ports.New([2]int{3100, 3990}), false)
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
