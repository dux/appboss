package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"dboss/internal/config"
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
	server, err := startHTTPServer("test", address, handler)
	if err != nil {
		t.Fatal(err)
	}
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

	if _, err := startHTTPServer("test", address, handler); err == nil {
		t.Fatal("binding an address already in use should fail")
	}
}
