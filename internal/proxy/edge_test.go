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

func TestTrustedOnlyRejectsOutsideCIDRs(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler, err := TrustedOnly([]string{"173.245.48.0/20", "2400:cb00::/32"}, next)
	if err != nil {
		t.Fatal(err)
	}
	for remote, want := range map[string]int{"173.245.48.9:1234": http.StatusOK, "[2400:cb00::1]:443": http.StatusOK, "203.0.113.9:1234": http.StatusForbidden} {
		request := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
		request.RemoteAddr = remote
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s: status = %d, want %d", remote, response.Code, want)
		}
	}
	if _, err := TrustedOnly([]string{"nope"}, next); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestCloudflareOnlyRejectsRequestsWithoutCFHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := CloudflareOnly(true, next)
	for name, headers := range map[string]map[string]string{
		"both":     {"CF-Ray": "abc123", "CF-Connecting-IP": "203.0.113.9"},
		"ray only": {"CF-Ray": "abc123"},
		"ip only":  {"CF-Connecting-IP": "203.0.113.9"},
		"neither":  {},
	} {
		request := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		want := http.StatusForbidden
		if name == "both" {
			want = http.StatusOK
		}
		if response.Code != want {
			t.Fatalf("%s: status = %d, want %d", name, response.Code, want)
		}
	}
	if CloudflareOnly(false, next) == nil {
		t.Fatal("disabled guard should still return a handler")
	}
}
