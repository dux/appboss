package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostSwitchRoutesManagementHost(t *testing.T) {
	management := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	apps := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := HostSwitch([]string{"boss.example.com", "boss.internal"}, management, apps)
	for host, want := range map[string]int{"boss.example.com": http.StatusAccepted, "Boss.Example.com:8080": http.StatusAccepted, "boss.internal": http.StatusAccepted, "app.example.com": http.StatusOK} {
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
