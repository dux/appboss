package proxy

import (
	"net"
	"net/http"
	"net/netip"

	"dboss/internal/config"
)

// HostSwitch serves the management console on any of its hostnames and everything else as
// app traffic.
func HostSwitch(managementHosts []string, management, apps http.Handler) http.Handler {
	hosts := make(map[string]bool, len(managementHosts))
	for _, host := range managementHosts {
		hosts[config.NormalizePattern(host)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hosts[config.NormalizeHost(r.Host)] {
			management.ServeHTTP(w, r)
			return
		}
		apps.ServeHTTP(w, r)
	})
}

// cloudflareRanges are the addresses Cloudflare connects from, as published at
// https://www.cloudflare.com/ips/.
var cloudflareRanges = []netip.Prefix{
	netip.MustParsePrefix("173.245.48.0/20"),
	netip.MustParsePrefix("103.21.244.0/22"),
	netip.MustParsePrefix("103.22.200.0/22"),
	netip.MustParsePrefix("103.31.4.0/22"),
	netip.MustParsePrefix("141.101.64.0/18"),
	netip.MustParsePrefix("108.162.192.0/18"),
	netip.MustParsePrefix("190.93.240.0/20"),
	netip.MustParsePrefix("188.114.96.0/20"),
	netip.MustParsePrefix("197.234.240.0/22"),
	netip.MustParsePrefix("198.41.128.0/17"),
	netip.MustParsePrefix("162.158.0.0/15"),
	netip.MustParsePrefix("104.16.0.0/13"),
	netip.MustParsePrefix("104.24.0.0/14"),
	netip.MustParsePrefix("172.64.0.0/13"),
	netip.MustParsePrefix("131.0.72.0/22"),
	netip.MustParsePrefix("2400:cb00::/32"),
	netip.MustParsePrefix("2606:4700::/32"),
	netip.MustParsePrefix("2803:f800::/32"),
	netip.MustParsePrefix("2405:b500::/32"),
	netip.MustParsePrefix("2405:8100::/32"),
	netip.MustParsePrefix("2a06:98c0::/29"),
	netip.MustParsePrefix("2c0f:f248::/32"),
	// Loopback, so a check or a tunnel on the box itself still reaches the proxy.
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
}

// CloudflareOnly refuses a connection that does not come from Cloudflare (or the box itself),
// so the origin cannot be reached directly and CF-Connecting-IP cannot be spoofed.
func CloudflareOnly(enabled bool, next http.Handler) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !inPrefixes(r.RemoteAddr, cloudflareRanges) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// inPrefixes reports whether an address, with or without a port, lies in any of prefixes.
func inPrefixes(address string, prefixes []netip.Prefix) bool {
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}
