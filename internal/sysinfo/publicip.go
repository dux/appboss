package sysinfo

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// echoURL is the outside observer a box behind NAT has to ask for its own address. It answers
// the address in plain text and nothing else. It is a variable so a test can point at a local
// server instead.
var echoURL = "https://api.ipify.org"

var echoClient = &http.Client{Timeout: 10 * time.Second}

// cgnat is the carrier-grade NAT range: globally unicast and not private, but never reachable
// from outside, which is also what a Tailscale address looks like.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// interfacePublicIP is the first routable address on this box's interfaces. On a server that is
// the address DNS points at, which is why it wins over asking an echo service. IPv4 is
// preferred, since an A record is what an operator usually needs.
func interfacePublicIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	var six string
	for _, addr := range addrs {
		network, ok := addr.(*net.IPNet)
		if !ok || !routable(network.IP) {
			continue
		}
		if v4 := network.IP.To4(); v4 != nil {
			return v4.String()
		}
		if six == "" {
			six = network.IP.String()
		}
	}
	return six
}

// routable is true for an address reachable from the internet. IsGlobalUnicast drops loopback,
// link-local and multicast, IsPrivate drops RFC 1918 and IPv6 ULA, and cgnat drops RFC 6598.
func routable(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	return !cgnat.Contains(ip)
}

// echoPublicIP asks the echo service what address this box arrives from. The answer is parsed
// as an IP, so an error page or a captive portal cannot end up on the Sys tab as one.
func echoPublicIP(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, echoURL, nil)
	if err != nil {
		return "", err
	}
	response, err := echoClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", echoURL, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64))
	if err != nil {
		return "", err
	}
	address := strings.TrimSpace(string(body))
	if net.ParseIP(address) == nil {
		return "", fmt.Errorf("%s answered %q, which is not an address", echoURL, address)
	}
	return address, nil
}
