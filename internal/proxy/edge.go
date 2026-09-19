package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// HostSwitch serves the management console on any of its hostnames and everything else as
// app traffic.
func HostSwitch(managementHosts []string, management, apps http.Handler) http.Handler {
	hosts := make(map[string]bool, len(managementHosts))
	for _, host := range managementHosts {
		hosts[strings.ToLower(host)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hosts[hostOnly(r.Host)] {
			management.ServeHTTP(w, r)
			return
		}
		apps.ServeHTTP(w, r)
	})
}

// TrustedOnly rejects connections whose source address is outside the given CIDRs.
// With Cloudflare ranges configured this stops direct origin access and client IP spoofing.
func TrustedOnly(cidrs []string, next http.Handler) (http.Handler, error) {
	if len(cidrs) == 0 {
		return next, nil
	}
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("trusted_cidrs: %w", err)
		}
		prefixes = append(prefixes, prefix)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !trusted(r.RemoteAddr, prefixes) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}

// CloudflareOnly rejects a request Cloudflare did not proxy, decided from the headers the edge
// always adds. It is the header-only alternative to trusted_cidrs; a direct caller can spoof the
// headers, so it pairs with a firewall that only lets Cloudflare connect.
func CloudflareOnly(enabled bool, next http.Handler) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("CF-Ray") == "" || r.Header.Get("CF-Connecting-IP") == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func trusted(remoteAddr string, prefixes []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	address = address.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func hostOnly(host string) string {
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}
