package proxy

import (
	"fmt"
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
		if !inPrefixes(r.RemoteAddr, prefixes) {
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
