package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"dboss/internal/config"
	"dboss/internal/supervisor"
)

// ACME terminates TLS with certificates obtained on demand. A host that resolves to one of the
// running apps or to the console gets a certificate the first time it is requested; no other
// name does, so a stranger cannot make dboss exhaust the certificate authority's rate limit.
type ACME struct {
	manager *autocert.Manager
}

// NewACME builds the certificate manager. The cache defaults to <state_dir>/acme; directory
// selects the authority (empty is Let's Encrypt production, staging its test endpoint, anything
// else is used as a directory URL). AcceptTOS is granted because the operator enables TLS.
func NewACME(cfg config.Config, manager *supervisor.Manager) (*ACME, error) {
	cache := cfg.Proxy.TLS.CacheDir
	if cache == "" {
		cache = filepath.Join(cfg.StateDir, "acme")
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return nil, fmt.Errorf("acme cache: %w", err)
	}
	acmeClient := &acme.Client{}
	switch strings.ToLower(strings.TrimSpace(cfg.Proxy.TLS.Directory)) {
	case "", "production":
		// Let's Encrypt production is acme.Client's default directory.
	case "staging":
		acmeClient.DirectoryURL = "https://acme-staging-v02.api.letsencrypt.org/directory"
	default:
		acmeClient.DirectoryURL = cfg.Proxy.TLS.Directory
	}
	m := &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		Cache:  autocert.DirCache(cache),
		Email:  cfg.Proxy.TLS.Email,
		Client: acmeClient,
		HostPolicy: func(_ context.Context, host string) error {
			if hostAllowed(cfg, manager, host) {
				return nil
			}
			return fmt.Errorf("dboss: no app or console is configured for %q", host)
		},
	}
	return &ACME{manager: m}, nil
}

// TLSConfig is the server TLS configuration: on-demand certificate lookup plus the ALPN
// protocol the ACME TLS-ALPN-01 challenge uses.
func (a *ACME) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: a.manager.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1", acme.ALPNProto},
	}
}

// HTTPHandler answers ACME HTTP-01 challenges on the plain-HTTP listener and redirects every
// other request to HTTPS. A non-nil fallback is served instead of the redirect for non-challenge
// requests, which is how a deployment that keeps plain HTTP keeps working.
func (a *ACME) HTTPHandler(fallback http.Handler) http.Handler {
	return a.manager.HTTPHandler(fallback)
}

// hostAllowed reports whether host belongs to an app of this host or to the console. It is the
// on-demand policy, evaluated against the live app table so a rescanned app needs no restart.
func hostAllowed(cfg config.Config, manager *supervisor.Manager, host string) bool {
	host = config.NormalizeHost(host)
	if host == "" {
		return false
	}
	for _, name := range cfg.Management.Host {
		if config.NormalizePattern(name) == host {
			return true
		}
	}
	if manager == nil {
		return false
	}
	_, ok := manager.ResolveHost(host)
	return ok
}
