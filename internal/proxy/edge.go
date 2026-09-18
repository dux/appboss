package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// HostSwitch serves the management console on its hostname and everything else as app traffic.
func HostSwitch(managementHost string, management, apps http.Handler) http.Handler {
	managementHost = strings.ToLower(managementHost)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hostOnly(r.Host) == managementHost {
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
