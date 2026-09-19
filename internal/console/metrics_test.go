package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dboss/internal/super"
)

func TestHealthAndReadyEndpoints(t *testing.T) {
	manager := &fakeManager{snapshots: []super.Snapshot{
		{Name: "web", State: super.Running, Autostart: true},
		{Name: "worker", State: super.Running, Autostart: true},
	}}
	handler := newTestHandler(t, manager, nil)

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != "ok\n" {
		t.Fatalf("healthz = %d %q", health.Code, health.Body.String())
	}

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("readyz = %d: %s", ready.Code, ready.Body.String())
	}
}

func TestReadyzFailsWhileAnAutostartAppIsDown(t *testing.T) {
	manager := &fakeManager{snapshots: []super.Snapshot{
		{Name: "web", State: super.Running, Autostart: true},
		{Name: "worker", State: super.Starting, Autostart: true},
		{Name: "optional", State: super.Stopped, Autostart: false},
	}}
	handler := newTestHandler(t, manager, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "worker=starting") {
		t.Fatalf("readyz body = %s", response.Body.String())
	}
}

func TestMetricsEndpointRendersAndChecksToken(t *testing.T) {
	manager := &fakeManager{snapshots: []super.Snapshot{{Name: "web", State: super.Running, Autostart: true}}}
	handler := newTestHandler(t, manager, nil)

	open := httptest.NewRecorder()
	handler.ServeHTTP(open, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/metrics", nil))
	if open.Code != http.StatusOK || !strings.Contains(open.Body.String(), `dboss_app_up{app="web"} 1`) {
		t.Fatalf("open metrics = %d: %s", open.Code, open.Body.String())
	}

	handler.metricsToken = "s3cret"
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/metrics", nil))
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless metrics = %d", denied.Code)
	}
	allowed := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/metrics", nil)
	allowed.Header.Set("Authorization", "Bearer s3cret")
	allowedResponse := httptest.NewRecorder()
	handler.ServeHTTP(allowedResponse, allowed)
	if allowedResponse.Code != http.StatusOK {
		t.Fatalf("bearer metrics = %d", allowedResponse.Code)
	}
}

func TestMetricsDisabledHidesEndpoints(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	handler.metricsEnabled = false
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081"+path, nil))
		if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "dboss_app_up") {
			t.Fatalf("%s = %d, want the endpoint disabled", path, response.Code)
		}
	}
}
