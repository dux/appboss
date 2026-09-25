package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/crypto/acme"

	"dboss/internal/config"
)

func TestHostAllowedAllowsConsoleAndRejectsUnknown(t *testing.T) {
	cfg := config.Default()
	cfg.Management.Host = config.List{"dboss.example.com"}
	for _, host := range []string{"dboss.example.com", "DBOSS.example.com:443", "dboss.example.com."} {
		if !hostAllowed(cfg, nil, host) {
			t.Fatalf("console host %q should be allowed", host)
		}
	}
	for _, host := range []string{"", "evil.com", "other.example.com"} {
		if hostAllowed(cfg, nil, host) {
			t.Fatalf("host %q should not be allowed", host)
		}
	}
}

func TestNewACMEInitializesCacheAndALPN(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Proxy.TLS.Listen = ":443"
	certs, err := NewACME(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "acme")); err != nil {
		t.Fatalf("certificate cache not created: %v", err)
	}
	if !slices.Contains(certs.TLSConfig().NextProtos, acme.ALPNProto) {
		t.Fatalf("ALPN challenge protocol missing from %v", certs.TLSConfig().NextProtos)
	}
}

func TestHTTPHandlerServesForwardedHTTPS(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Proxy.TLS.Listen = ":443"
	certs, err := NewACME(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := certs.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	forwarded := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	forwarded.Header.Set("X-Forwarded-Proto", "https")
	secure := httptest.NewRecorder()
	handler.ServeHTTP(secure, forwarded)
	if secure.Code != http.StatusTeapot {
		t.Fatalf("forwarded https should reach the fallback, got %d", secure.Code)
	}

	plain := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	redirected := httptest.NewRecorder()
	handler.ServeHTTP(redirected, plain)
	if redirected.Code != http.StatusFound {
		t.Fatalf("plain http should redirect, got %d", redirected.Code)
	}
}
