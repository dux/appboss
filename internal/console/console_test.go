package console

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"deploy-boss/internal/apps"
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

func (m *fakeManager) SetMaintenance(app string, on bool) error {
	m.actions = append(m.actions, fmt.Sprintf("maintenance %s %v", app, on))
	return nil
}

func (m *fakeManager) Rescan() ([]error, error) {
	m.actions = append(m.actions, "rescan")
	return m.warnings, nil
}

func (m *fakeManager) RestartRequired() []string { return nil }

// fakeStore keeps the files in memory but follows the real store's contract: revisions are
// hashes of the contents and a stale revision is a conflict.
type fakeStore struct {
	files   map[string]*apps.ConfigFile
	invalid map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{files: map[string]*apps.ConfigFile{
		"host":        {ID: "host", Path: "/srv/dboss.yaml", Source: "dboss.yaml", Contents: "apps: ./apps\n"},
		"app:sinatra": {ID: "app:sinatra", App: "sinatra", Path: "/srv/apps/sinatra/dboss.yaml", Source: "dboss.yaml", Contents: "procfile:\n  web: ./server\n"},
	}, invalid: map[string]string{}}
}

func (s *fakeStore) revision(contents string) string {
	sum := sha256.Sum256([]byte(contents))
	return hex.EncodeToString(sum[:])
}

func (s *fakeStore) Files() ([]apps.ConfigFile, error) {
	var files []apps.ConfigFile
	for _, id := range []string{"host", "app:sinatra"} {
		file := *s.files[id]
		file.Revision, file.Contents = s.revision(file.Contents), ""
		files = append(files, file)
	}
	return files, nil
}

func (s *fakeStore) Read(id string) (apps.ConfigFile, error) {
	file, ok := s.files[id]
	if !ok {
		return apps.ConfigFile{}, errors.New("unknown config file")
	}
	result := *file
	result.Revision = s.revision(result.Contents)
	return result, nil
}

func (s *fakeStore) Validate(id, contents string) error {
	if message, bad := s.invalid[contents]; bad {
		return errors.New(message)
	}
	return nil
}

func (s *fakeStore) Write(id, contents, revision string) (apps.ConfigFile, error) {
	current, err := s.Read(id)
	if err != nil {
		return apps.ConfigFile{}, err
	}
	if current.Revision != revision {
		return current, apps.ErrConflict
	}
	if err := s.Validate(id, contents); err != nil {
		return apps.ConfigFile{}, err
	}
	s.files[id].Contents = contents
	return s.Read(id)
}

func (s *fakeStore) CreateLocal(app string) (apps.ConfigFile, error) {
	file := s.files["app:"+app]
	if file == nil || file.HasLocal {
		return apps.ConfigFile{}, errors.New("cannot create override")
	}
	file.HasLocal, file.Source, file.Path = true, "dboss.local.yaml", "/srv/apps/sinatra/dboss.local.yaml"
	return s.Read("app:" + app)
}

func (s *fakeStore) Effective(app string) (string, error) {
	if app != "sinatra" {
		return "", errors.New("unknown app")
	}
	return "procfile:\n  web: ./server\nidle_stop: 6h0m0s\n", nil
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
	cfg.Management.Host = config.List{"boss.lvh.me", "boss.internal"}
	cfg.Management.Auth.AdminEmails = []string{"admin@example.com"}
	handler, err := New(cfg, manager, rates, newFakeStore())
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestConsoleAnswersForEveryManagementHost(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	cookie, _ := sessionCookie(t, handler)
	for host, want := range map[string]int{"boss.lvh.me:8081": http.StatusOK, "boss.internal": http.StatusOK, "other.lvh.me:8081": http.StatusNotFound} {
		request := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/apps", nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s: status = %d, want %d", host, response.Code, want)
		}
	}
}

// call sends an authenticated JSON request with the CSRF headers the console requires.
func call(t *testing.T, handler *Handler, cookie *http.Cookie, session authSession, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, "http://boss.lvh.me:8081"+target, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Origin", "http://boss.lvh.me:8081")
	request.Header.Set("X-CSRF-Token", session.CSRF)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestConsoleConfigEditorRoundTrip(t *testing.T) {
	manager := &fakeManager{warnings: []error{errors.New("bun: procfile.web command is empty")}}
	handler := newTestHandler(t, manager, nil)
	store := handler.store.(*fakeStore)
	store.invalid["broken"] = "decode dboss.yaml: yaml: line 3: mapping values are not allowed in this context"
	cookie, session := sessionCookie(t, handler)

	list := call(t, handler, cookie, session, http.MethodGet, "/api/config", "")
	var listed struct{ Files []apps.ConfigFile }
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil || len(listed.Files) != 2 || listed.Files[1].Contents != "" || listed.Files[1].Revision == "" {
		t.Fatalf("unexpected list: %d %s %v", list.Code, list.Body.String(), err)
	}
	read := call(t, handler, cookie, session, http.MethodGet, "/api/config/file?id=app:sinatra", "")
	var file apps.ConfigFile
	if err := json.Unmarshal(read.Body.Bytes(), &file); err != nil || file.Contents == "" || file.Revision != listed.Files[1].Revision {
		t.Fatalf("unexpected read: %d %s", read.Code, read.Body.String())
	}

	validate := call(t, handler, cookie, session, http.MethodPost, "/api/config/validate", `{"id":"app:sinatra","contents":"broken"}`)
	var validated validateResponse
	if err := json.Unmarshal(validate.Body.Bytes(), &validated); err != nil || validated.OK || validated.Line != 3 {
		t.Fatalf("unexpected validate: %d %s", validate.Code, validate.Body.String())
	}

	stale := call(t, handler, cookie, session, http.MethodPut, "/api/config/file", `{"id":"app:sinatra","contents":"procfile:\n  web: ./other\n","revision":"stale"}`)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"contents":"procfile:\n  web: ./server\n"`) {
		t.Fatalf("stale write should answer 409 with the current file: %d %s", stale.Code, stale.Body.String())
	}
	written := call(t, handler, cookie, session, http.MethodPut, "/api/config/file", `{"id":"app:sinatra","contents":"procfile:\n  web: ./other\n","revision":"`+file.Revision+`"}`)
	var result writeResponse
	if err := json.Unmarshal(written.Body.Bytes(), &result); err != nil || written.Code != http.StatusOK || result.File.Revision == file.Revision || len(result.Invalid) != 1 || manager.actions[len(manager.actions)-1] != "rescan" {
		t.Fatalf("unexpected write: %d %s actions=%v", written.Code, written.Body.String(), manager.actions)
	}

	local := call(t, handler, cookie, session, http.MethodPost, "/api/config/local", `{"app":"sinatra"}`)
	if local.Code != http.StatusOK || !strings.Contains(local.Body.String(), `"source":"dboss.local.yaml"`) {
		t.Fatalf("unexpected override: %d %s", local.Code, local.Body.String())
	}
	effective := call(t, handler, cookie, session, http.MethodGet, "/api/config/effective?app=sinatra", "")
	if effective.Code != http.StatusOK || !strings.Contains(effective.Body.String(), "idle_stop") {
		t.Fatalf("unexpected effective config: %d %s", effective.Code, effective.Body.String())
	}
	reference := call(t, handler, cookie, session, http.MethodGet, "/api/config/reference", "")
	if reference.Code != http.StatusOK || !strings.Contains(reference.Body.String(), "PART 1") {
		t.Fatalf("unexpected reference: %d", reference.Code)
	}

	noCSRF := httptest.NewRequest(http.MethodPut, "http://boss.lvh.me:8081/api/config/file", strings.NewReader(`{"id":"host","contents":"","revision":""}`))
	noCSRF.Header.Set("Content-Type", "application/json")
	noCSRF.AddCookie(cookie)
	noCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(noCSRFResponse, noCSRF)
	if noCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("write without CSRF token: %d", noCSRFResponse.Code)
	}
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
