package config

import (
	"net"
	"strings"
)

// NormalizeHost lowercases a request host and strips its port and any trailing dot, the form every
// host pattern is matched against. A bracketed IPv6 literal keeps its address.
func NormalizeHost(host string) string {
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	return NormalizePattern(host)
}

// NormalizePattern is the case- and trailing-dot-insensitive form of a hostname or hosts pattern.
func NormalizePattern(pattern string) string {
	return strings.ToLower(strings.TrimSuffix(pattern, "."))
}

// MatchHost scores a normalized host against a hosts pattern. A leading "*." matches subdomains
// only, a leading "." matches the bare domain and every subdomain, and a plain pattern must be an
// exact match. The score is the pattern length, so a longer, more specific match wins.
func MatchHost(host, pattern string) (int, bool) {
	pattern = NormalizePattern(pattern)
	if host == pattern {
		return len(pattern) + 10000, true
	}
	if strings.HasPrefix(pattern, ".") {
		if host == pattern[1:] {
			return len(pattern) + 10000, true
		}
		return len(pattern), strings.HasSuffix(host, pattern) && len(host) > len(pattern)
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		return len(suffix), strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	return 0, false
}

// BestMatch is the highest MatchHost score of host against any of patterns.
func BestMatch(host string, patterns []string) (int, bool) {
	best, found := 0, false
	for _, pattern := range patterns {
		if score, ok := MatchHost(host, pattern); ok && (!found || score > best) {
			best, found = score, true
		}
	}
	return best, found
}

// PrimaryHost is the one concrete hostname a web process answers on, for links, example URLs and
// the health probe's Host header: the canonical host when set (every other host redirects to it),
// then the first concrete host, then a leading-dot pattern's apex. A process with only "*."
// patterns has none and gets "".
func PrimaryHost(canonical string, hosts []string) string {
	if canonical != "" {
		return canonical
	}
	for _, host := range hosts {
		if !strings.HasPrefix(host, "*.") && !strings.HasPrefix(host, ".") {
			return host
		}
	}
	for _, host := range hosts {
		if apex, ok := strings.CutPrefix(host, "."); ok {
			return apex
		}
	}
	return ""
}

func hostAllowed(host string, patterns []string) bool {
	_, ok := BestMatch(NormalizePattern(host), patterns)
	return ok
}
