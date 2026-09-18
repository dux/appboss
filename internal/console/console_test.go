package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"deploy-boss/internal/config"
	"deploy-boss/internal/reqlog"
	"deploy-boss/internal/super"
)

type fakeManager struct {
	snapshots []super.Snapshot
	logs      map[string][]string
	actions   []string
	warnings  []error
}

func (m *fakeManager) Snapshots() []super.Snapshot {
	return append([]super.Snapshot(nil), m.snapshots...)
}

func (m *fakeManager) Logs(app, process string, lines int) (map[string][]string, error) {
	if m.logs == nil {
		return nil, errors.New("missing log fixture")
	}
	return m.logs, nil
}

func (m *fakeManager) Start(app string) error {
	m.actions = append(m.actions, "start "+app)
	return nil
}

func (m *fakeManager) Stop(app string) error {
	m.actions = append(m.actions, "stop "+app)
	return nil
}

func (m *fakeManager) Restart(app string) error {
	m.actions = append(m.actions, "restart "+app)
	return nil
}

func (m *fakeManager) Rescan() ([]error, error) {
	m.actions = append(m.actions, "rescan")
	return m.warnings, nil
}

type fakeRates map[string]reqlog.Rates

func (rates fakeRates) Rates(app string) (reqlog.Rates, error) {
	if value, ok := rates[app]; ok {
		return value, nil
	}
	return reqlog.Rates{}, errors.New("missing rate fixture")
}

func TestConsoleBootstrapAndActions(t *testing.T) {
	manager := &fakeManager{snapshots: []super.Snapshot{{Name: "sinatra", State: super.Running, Hosts: []string{"sinatra.lvh.me"}}}}
	handler := newTestHandler(t, manager, fakeRates{"sinatra": {LastMinute: 2, LastHour: 7, LastDay: 20}})
	cookie, session := sessionCookie(t, handler)

	bootstrapRequest := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/api/bootstrap", nil)
	bootstrapRequest.AddCookie(cookie)
	bootstrapResponse := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapResponse, bootstrapRequest)
	if bootstrapResponse.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d: %s", bootstrapResponse.Code, bootstrapResponse.Body.String())
	}
	var dashboard dashboard
	if err := json.Unmarshal(bootstrapResponse.Body.Bytes(), &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.Viewer != "admin@example.com" || dashboard.CSRF != session.CSRF || dashboard.Apps[0].RequestRates.LastHour != 7 {
		t.Fatalf("unexpected dashboard: %+v", dashboard)
	}

	actionRequest := httptest.NewRequest(http.MethodPost, "http://boss.lvh.me:8081/api/action", strings.NewReader(`{"app":"sinatra","action":"restart"}`))
	actionRequest.Header.Set("Content-Type", "application/json")
	actionRequest.Header.Set("Origin", "http://boss.lvh.me:8081")
	actionRequest.Header.Set("X-CSRF-Token", session.CSRF)
	actionRequest.AddCookie(cookie)
	actionResponse := httptest.NewRecorder()
	handler.ServeHTTP(actionResponse, actionRequest)
	if actionResponse.Code != http.StatusOK || len(manager.actions) != 1 || manager.actions[0] != "restart sinatra" {
		t.Fatalf("unexpected action response: %d %v %s", actionResponse.Code, manager.actions, actionResponse.Body.String())
	}
}

func TestConsoleServesAppLogsWithoutRefresh(t *testing.T) {
	manager := &fakeManager{logs: map[string][]string{"web": {"hello", "world"}}}
	handler := newTestHandler(t, manager, nil)
	cookie, _ := sessionCookie(t, handler)
	request := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/logs?app=sinatra", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" || response.Body.String() != "hello\nworld" {
		t.Fatalf("unexpected logs response: %d %s", response.Code, response.Body.String())
	}
}

func TestConsoleRejectsWrongHostAndMissingCSRF(t *testing.T) {
	manager := &fakeManager{}
	handler := newTestHandler(t, manager, nil)
	cookie, _ := sessionCookie(t, handler)

	wrongHost := httptest.NewRequest(http.MethodGet, "http://sinatra.lvh.me:8081/", nil)
	wrongHost.AddCookie(cookie)
	wrongHostResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongHostResponse, wrongHost)
	if wrongHostResponse.Code != http.StatusNotFound {
		t.Fatalf("wrong host status = %d", wrongHostResponse.Code)
	}

	actionRequest := httptest.NewRequest(http.MethodPost, "http://boss.lvh.me:8081/api/action", strings.NewReader(`{"app":"sinatra","action":"start"}`))
	actionRequest.Header.Set("Content-Type", "application/json")
	actionRequest.AddCookie(cookie)
	actionResponse := httptest.NewRecorder()
	handler.ServeHTTP(actionResponse, actionRequest)
	if actionResponse.Code != http.StatusForbidden || len(manager.actions) != 0 {
		t.Fatalf("missing CSRF response: %d %v", actionResponse.Code, manager.actions)
	}
}

func TestConsoleAssetsRequireAuthentication(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	request := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusFound || !strings.Contains(response.Header().Get("Location"), "auth.authcog.com") {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Header().Get("Location"))
	}
}

func TestConsoleServesAuthenticatedRoot(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	cookie, _ := sessionCookie(t, handler)
	request := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Deploy Boss") {
		t.Fatalf("unexpected root response: %d %s", response.Code, response.Body.String())
	}
}

func newTestHandler(t *testing.T, manager AppManager, rates RateReader) *Handler {
	t.Helper()
	cfg := config.Default()
	cfg.Apps = "/apps"
	cfg.StateDir = t.TempDir()
	cfg.Management.Host = "boss.lvh.me"
	cfg.Management.Auth.AdminEmails = []string{"admin@example.com"}
	handler, err := New(cfg, manager, rates)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func sessionCookie(t *testing.T, handler *Handler) (*http.Cookie, authSession) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/", nil)
	response := httptest.NewRecorder()
	if err := handler.auth.setSessionCookie(response, request, "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	cookie := cookieNamed(t, response.Result().Cookies(), authSessionCookie)
	authorized := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/", nil)
	authorized.AddCookie(cookie)
	session, ok := handler.auth.validSession(authorized)
	if !ok || session.ExpiresAt <= time.Now().Unix() {
		t.Fatal("generated session is invalid")
	}
	return cookie, session
}
