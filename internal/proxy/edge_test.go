package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostSwitchRoutesManagementHost(t *testing.T) {
	management := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	apps := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := HostSwitch([]string{"dboss.example.com", "dboss.internal"}, management, apps)
	for host, want := range map[string]int{"dboss.example.com": http.StatusAccepted, "dboss.Example.com:8080": http.StatusAccepted, "dboss.internal": http.StatusAccepted, "app.example.com": http.StatusOK} {
		request := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s: status = %d, want %d", host, response.Code, want)
		}
	}
}

func TestCloudflareOnlyAcceptsCloudflareAndLoopback(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := CloudflareOnly(true, next)
	for remote, want := range map[string]int{
		"173.245.48.9:1234":  http.StatusOK,
		"[2400:cb00::1]:443": http.StatusOK,
		"127.0.0.1:5555":     http.StatusOK,
		"[::1]:5555":         http.StatusOK,
		"203.0.113.9:1234":   http.StatusForbidden,
	} {
		request := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
		request.RemoteAddr = remote
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s: status = %d, want %d", remote, response.Code, want)
		}
	}
	open := CloudflareOnly(false, next)
	request := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
	request.RemoteAddr = "203.0.113.9:1234"
	response := httptest.NewRecorder()
	open.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("disabled guard answered %d", response.Code)
	}
}

func TestClientIPTrustsTheHeaderOnlyBehindCloudflare(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
	request.RemoteAddr = "173.245.48.9:1234"
	request.Header.Set("CF-Connecting-IP", "203.0.113.9")
	request.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := clientIP(request, true); got != "203.0.113.9" {
		t.Fatalf("behind cloudflare = %q", got)
	}
	if got := clientIP(request, false); got != "173.245.48.9" {
		t.Fatalf("direct = %q", got)
	}
}
