package sysinfo

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoutableRejectsUnreachableRanges(t *testing.T) {
	for address, want := range map[string]bool{
		"93.184.216.34":   true,
		"2606:2800:220::": true,
		"10.0.0.7":        false,
		"172.16.4.4":      false,
		"192.168.1.10":    false,
		"100.101.102.103": false, // carrier NAT, which is also what Tailscale hands out
		"127.0.0.1":       false,
		"169.254.10.1":    false,
		"fd00::1":         false,
		"::1":             false,
	} {
		if got := routable(net.ParseIP(address)); got != want {
			t.Errorf("routable(%s) = %v, want %v", address, got, want)
		}
	}
	if routable(nil) {
		t.Error("a missing address is not routable")
	}
}

func TestEchoPublicIPReadsTheAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "203.0.113.7")
	}))
	defer server.Close()

	previous := echoURL
	echoURL = server.URL
	defer func() { echoURL = previous }()

	address, err := echoPublicIP(context.Background())
	if err != nil || address != "203.0.113.7" {
		t.Fatalf("echoPublicIP = %q, %v", address, err)
	}
}

// A captive portal or an error page answers 200 with HTML; it must not land on the Sys tab as
// an address.
func TestEchoPublicIPRejectsNonAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>sign in to continue</html>")
	}))
	defer server.Close()

	previous := echoURL
	echoURL = server.URL
	defer func() { echoURL = previous }()

	if address, err := echoPublicIP(context.Background()); err == nil {
		t.Fatalf("junk was accepted as %q", address)
	}
}

// The echo service is only asked when the box has no routable address of its own.
func TestPublicIPPrefersTheInterface(t *testing.T) {
	inspector := NewInspector(nil)
	asked := 0
	inspector.echo = newRemote(func(context.Context) (string, error) {
		asked++
		return "203.0.113.7", nil
	})

	address := inspector.publicIP(context.Background())
	if local := interfacePublicIP(); local != "" {
		if address != local || asked != 0 {
			t.Fatalf("publicIP = %q with %d echo lookups, want the interface address %q", address, asked, local)
		}
		return
	}
	if address != "203.0.113.7" || asked != 1 {
		t.Fatalf("publicIP = %q with %d echo lookups, want the echoed address", address, asked)
	}
}
