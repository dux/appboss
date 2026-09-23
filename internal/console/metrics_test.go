package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dboss/internal/supervisor"
)

func TestHealthAndReadyEndpoints(t *testing.T) {
	manager := &fakeManager{snapshots: []supervisor.Snapshot{
		{Name: "web", State: supervisor.Running, Autostart: true},
		{Name: "worker", State: supervisor.Running, Autostart: true},
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
	manager := &fakeManager{snapshots: []supervisor.Snapshot{
		{Name: "web", State: supervisor.Running, Autostart: true},
		{Name: "worker", State: supervisor.Starting, Autostart: true},
		{Name: "optional", State: supervisor.Stopped, Autostart: false},
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

func TestReadyzCountsASleepingAutostartAppAsReady(t *testing.T) {
	manager := &fakeManager{snapshots: []supervisor.Snapshot{
		{Name: "web", State: supervisor.Running, Autostart: true},
		{Name: "sleepy", State: supervisor.Stopped, Autostart: true},
	}}
	handler := newTestHandler(t, manager, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("readyz = %d: %s", response.Code, response.Body.String())
	}
}

func TestMetricsEndpointNeedsTheDbossToken(t *testing.T) {
	manager := &fakeManager{snapshots: []supervisor.Snapshot{{Name: "web", State: supervisor.Running, Autostart: true}}}
	handler := newTestHandler(t, manager, nil)

	hidden := httptest.NewRecorder()
	handler.ServeHTTP(hidden, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/metrics", nil))
	if hidden.Code != http.StatusNotFound || !strings.Contains(hidden.Body.String(), "set tokens.dboss") {
		t.Fatalf("metrics without tokens.dboss = %d, want 404", hidden.Code)
	}

	manager.token = "s3cret"
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/metrics", nil))
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless metrics = %d", denied.Code)
	}
	allowed := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/metrics", nil)
	allowed.Header.Set("Authorization", "Bearer s3cret")
	allowedResponse := httptest.NewRecorder()
	handler.ServeHTTP(allowedResponse, allowed)
	if allowedResponse.Code != http.StatusOK || !strings.Contains(allowedResponse.Body.String(), `dboss_app_up{app="web"} 1`) {
		t.Fatalf("bearer metrics = %d: %s", allowedResponse.Code, allowedResponse.Body.String())
	}
}
