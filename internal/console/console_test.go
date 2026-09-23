package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dboss/internal/apps"
	"dboss/internal/authcog"
	"dboss/internal/config"
	"dboss/internal/diskusage"
	"dboss/internal/logstore"
	"dboss/internal/ops"
	"dboss/internal/supervisor"
	"dboss/internal/sysinfo"
	"dboss/internal/version"
)

// fakeSys is a SysReader whose refresh is observable.
type fakeSys struct {
	snapshot  sysinfo.Snapshot
	refreshes int
}

func (f *fakeSys) Snapshot() sysinfo.Snapshot { return f.snapshot }

func (f *fakeSys) Refresh(context.Context) sysinfo.Snapshot {
	f.refreshes++
	f.snapshot.Host.Hostname = "refreshed"
	return f.snapshot
}

type fakeManager struct {
	snapshots   []supervisor.Snapshot
	logs        map[string][]string
	actions     []string
	warnings    []error
	hooks       map[string][]supervisor.HookInfo
	hookSecrets map[string]string
	// token is tokens.dboss of the fake host config.
	token string
}

func (m *fakeManager) Snapshots() []supervisor.Snapshot {
	return append([]supervisor.Snapshot(nil), m.snapshots...)
}

func (m *fakeManager) Snapshot(app string) (supervisor.Snapshot, error) {
	for _, snapshot := range m.snapshots {
		if snapshot.Name == app {
			return snapshot, nil
		}
	}
	return supervisor.Snapshot{}, errors.New("unknown app")
}

func (m *fakeManager) Ports() map[string]int { return map[string]int{} }

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

func (m *fakeManager) Destroy(app string) error {
	m.actions = append(m.actions, "destroy "+app)
	return nil
}

func (m *fakeManager) SetMaintenance(app string, on bool) error {
	m.actions = append(m.actions, fmt.Sprintf("maintenance %s %v", app, on))
	return nil
}

func (m *fakeManager) RunCron(app, job string) error {
	m.actions = append(m.actions, fmt.Sprintf("cron-run %s %s", app, job))
	return nil
}

func (m *fakeManager) RunHook(app, hook string) error {
	m.actions = append(m.actions, fmt.Sprintf("hook-run %s %s", app, hook))
	return nil
}

func (m *fakeManager) Hooks(app string) ([]supervisor.HookInfo, error) {
	return m.hooks[app], nil
}

func (m *fakeManager) HookToken(app, hook string) (string, error) {
	secret, ok := m.hookSecrets[app+"/"+hook]
	if !ok {
		return "", errors.New("unknown hook")
	}
	return secret, nil
}

func (m *fakeManager) HostHookToken(name string) (string, error) {
	return "host-secret", nil
}

func (m *fakeManager) Exec(app string, argv []string, timeout time.Duration) (supervisor.ExecResult, error) {
	m.actions = append(m.actions, fmt.Sprintf("exec %s %s", app, strings.Join(argv, " ")))
	return supervisor.ExecResult{Output: "ran\n", ExitCode: 0}, nil
}

func (m *fakeManager) Rescan() ([]error, error) {
	m.actions = append(m.actions, "rescan")
	return m.warnings, nil
}

func (m *fakeManager) RestartRequired() []string { return nil }
func (m *fakeManager) HostConfig() config.Config {
	cfg := config.Default()
	cfg.Tokens.Dboss = m.token
	return cfg
}

// fakeStore keeps the files in memory but follows the real store's contract: revisions are
// hashes of the contents and a stale revision is a conflict.
type fakeStore struct {
	files           map[string]*apps.ConfigFile
	invalid         map[string]string
	history         map[string][]apps.ConfigRevision
	historyContents map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{files: map[string]*apps.ConfigFile{
		"host":        {ID: "host", Path: "/srv/dboss.yaml", Source: "dboss.yaml", Contents: "apps: ./apps\n"},
		"app:sinatra": {ID: "app:sinatra", App: "sinatra", Path: "/srv/apps/sinatra/dboss.yaml", Source: "dboss.yaml", Contents: "procfile:\n  web: ./server\n"},
	}, invalid: map[string]string{}, history: map[string][]apps.ConfigRevision{}, historyContents: map[string]string{}}
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

func (s *fakeStore) EnsureLocal(app string) (apps.ConfigFile, error) {
	file := s.files["app:"+app]
	if file == nil {
		return apps.ConfigFile{}, errors.New("unknown config file")
	}
	if file.HasLocal {
		return s.Read("app:" + app)
	}
	return s.CreateLocal(app)
}

func (s *fakeStore) CreateHostLocal() (apps.ConfigFile, error) { return s.Read("host") }

func (s *fakeStore) Effective(app string) (string, error) {
	if app != "sinatra" {
		return "", errors.New("unknown app")
	}
	return "procfile:\n  web: ./server\nidle_stop: 6h0m0s\n", nil
}

func (s *fakeStore) History(id string) ([]apps.ConfigRevision, error) {
	if _, ok := s.files[id]; !ok {
		return nil, errors.New("unknown config file")
	}
	return s.history[id], nil
}

func (s *fakeStore) HistoryContents(id, revision string) (string, error) {
	contents, ok := s.historyContents[id+"/"+revision]
	if !ok {
		return "", errors.New("unknown revision")
	}
	return contents, nil
}

func (s *fakeStore) Restore(id, revision string) (apps.ConfigFile, error) {
	contents, err := s.HistoryContents(id, revision)
	if err != nil {
		return apps.ConfigFile{}, err
	}
	current, err := s.Read(id)
	if err != nil {
		return apps.ConfigFile{}, err
	}
	return s.Write(id, contents, current.Revision)
}

type fakeRates map[string]logstore.Rates

// fakeLogs is the log store the console tests read; rates answers the request counters.
type fakeLogs struct{ rates fakeRates }

func (f fakeLogs) Rates(app string) (logstore.Rates, error) {
	if value, ok := f.rates[app]; ok {
		return value, nil
	}
	return logstore.Rates{}, errors.New("missing rate fixture")
}

func (fakeLogs) Window(string, time.Time) (logstore.Window, error) { return logstore.Window{}, nil }

func (fakeLogs) SearchLogs(string, logstore.LogFilter) ([]logstore.LogEntry, error) {
	return []logstore.LogEntry{{Time: time.Now(), Source: "process", Process: "web", Level: "error", Message: "boom"}}, nil
}

func (fakeLogs) SearchRequests(string, logstore.RequestFilter) ([]logstore.RequestEntry, error) {
	return []logstore.RequestEntry{{Time: time.Now(), Method: "GET", Path: "/hello", Status: 200, Country: "HR"}}, nil
}

func (fakeLogs) Traffic(app string, since time.Time) (logstore.Traffic, error) {
	return logstore.Traffic{Since: since, Bucket: "hour", Totals: logstore.Window{Count: 42}, Paths: []logstore.TrafficPath{{Path: "/hello", Count: 42}}}, nil
}

func (fakeLogs) Channels(string) ([]logstore.Channel, error) {
	return []logstore.Channel{{ID: "request", Label: "REQUEST"}, {ID: "stdout", Label: "STDOUT"}, {ID: "file:production.log", Label: "production.log"}}, nil
}

func (fakeLogs) Tree([]string) ([]logstore.AppTree, error) {
	return []logstore.AppTree{{
		Name:     "sinatra",
		Bytes:    4096,
		Channels: []logstore.Channel{{ID: "request", Label: "REQUEST"}, {ID: "stdout", Label: "STDOUT"}, {ID: "file:production.log", Label: "production.log"}},
	}, {
		Name:     logstore.HostApp,
		Bytes:    1024,
		Channels: []logstore.Channel{{ID: "dboss", Label: "dboss"}},
	}}, nil
}

func (fakeLogs) RecordAudit(logstore.AuditEntry) error { return nil }

func (fakeLogs) SearchAudit(logstore.AuditFilter) ([]logstore.AuditEntry, error) {
	return []logstore.AuditEntry{{Time: time.Now(), Actor: "admin@example.com", App: "sinatra", Action: "restart", Result: "ok"}}, nil
}

func TestConsoleBootstrapAndActions(t *testing.T) {
	manager := &fakeManager{snapshots: []supervisor.Snapshot{{Name: "sinatra", State: supervisor.Running, Hosts: []string{"sinatra.lvh.me"}}}}
	handler := newTestHandler(t, manager, fakeRates{"sinatra": {LastMinute: 2, LastHour: 7, LastDay: 20}})
	cookie, session := sessionCookie(t, handler)

	bootstrapRequest := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/api/bootstrap", nil)
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
	// The navbar renders this next to the brand, so an empty payload would leave it blank.
	if dashboard.Version != version.String() {
		t.Fatalf("dashboard version = %q, want %q", dashboard.Version, version.String())
	}

	actionRequest := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/api/action", strings.NewReader(`{"app":"sinatra","action":"restart"}`))
	actionRequest.Header.Set("Content-Type", "application/json")
	actionRequest.Header.Set("Origin", "http://dboss.lvh.me:8081")
	actionRequest.Header.Set("X-CSRF-Token", session.CSRF)
	actionRequest.AddCookie(cookie)
	actionResponse := httptest.NewRecorder()
	handler.ServeHTTP(actionResponse, actionRequest)
	if actionResponse.Code != http.StatusOK || len(manager.actions) != 1 || manager.actions[0] != "restart sinatra" {
		t.Fatalf("unexpected action response: %d %v %s", actionResponse.Code, manager.actions, actionResponse.Body.String())
	}

	destroyRequest := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/api/action", strings.NewReader(`{"app":"sinatra","action":"destroy"}`))
	destroyRequest.Header.Set("Content-Type", "application/json")
	destroyRequest.Header.Set("Origin", "http://dboss.lvh.me:8081")
	destroyRequest.Header.Set("X-CSRF-Token", session.CSRF)
	destroyRequest.AddCookie(cookie)
	destroyResponse := httptest.NewRecorder()
	handler.ServeHTTP(destroyResponse, destroyRequest)
	if destroyResponse.Code != http.StatusOK || len(manager.actions) != 2 || manager.actions[1] != "destroy sinatra" {
		t.Fatalf("unexpected destroy response: %d %v %s", destroyResponse.Code, manager.actions, destroyResponse.Body.String())
	}
}

func TestConsoleRunsCronJob(t *testing.T) {
	manager := &fakeManager{snapshots: []supervisor.Snapshot{{Name: "bun"}}}
	handler := newTestHandler(t, manager, nil)
	cookie, session := sessionCookie(t, handler)
	request := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/api/action", strings.NewReader(`{"app":"bun","action":"cron-run","job":"heartbeat"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://dboss.lvh.me:8081")
	request.Header.Set("X-CSRF-Token", session.CSRF)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(manager.actions) != 1 || manager.actions[0] != "cron-run bun heartbeat" {
		t.Fatalf("unexpected response: %d %v %s", response.Code, manager.actions, response.Body.String())
	}
}

func TestConsoleServesFavicon(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	cookie, _ := sessionCookie(t, handler)
	request := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/favicon.ico", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(response.Body.String(), "<svg") {
		t.Fatalf("unexpected favicon: %d %s", response.Code, response.Header().Get("Content-Type"))
	}
	if csp := response.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self'") {
		t.Fatalf("favicon needs img-src 'self': %s", csp)
	}
}

func TestConsoleServesShellAndLogTextExport(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	cookie, _ := sessionCookie(t, handler)
	page := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/?app=sinatra", nil)
	page.AddCookie(cookie)
	pageResponse := httptest.NewRecorder()
	handler.ServeHTTP(pageResponse, page)
	if pageResponse.Code != http.StatusOK || !strings.Contains(pageResponse.Body.String(), "route-outlet") {
		t.Fatalf("unexpected shell page: %d %s", pageResponse.Code, pageResponse.Body.String())
	}

	text := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/logs.txt?app=sinatra&channel=stdout", nil)
	text.AddCookie(cookie)
	textResponse := httptest.NewRecorder()
	handler.ServeHTTP(textResponse, text)
	if textResponse.Code != http.StatusOK || textResponse.Header().Get("Content-Type") != "text/plain; charset=utf-8" || !strings.Contains(textResponse.Body.String(), "boom") {
		t.Fatalf("unexpected logs response: %d %s", textResponse.Code, textResponse.Body.String())
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

	actionRequest := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/api/action", strings.NewReader(`{"app":"sinatra","action":"start"}`))
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
	request := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusFound || !strings.Contains(response.Header().Get("Location"), "auth.authcog.com") {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Header().Get("Location"))
	}
}

func TestConsoleServesAuthenticatedRoot(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	cookie, _ := sessionCookie(t, handler)
	request := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "dboss") {
		t.Fatalf("unexpected root response: %d %s", response.Code, response.Body.String())
	}
}

func TestConsoleServesLogAndRequestSearch(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	cookie, session := sessionCookie(t, handler)

	logs := call(t, handler, cookie, session, http.MethodGet, "/api/log/search?app=sinatra&channel=stdout&level=error", "")
	if logs.Code != http.StatusOK || !strings.Contains(logs.Body.String(), `"kind":"log"`) || !strings.Contains(logs.Body.String(), `"message":"boom"`) {
		t.Fatalf("unexpected logs: %d %s", logs.Code, logs.Body.String())
	}
	requests := call(t, handler, cookie, session, http.MethodGet, "/api/log/search?app=sinatra&channel=request", "")
	if requests.Code != http.StatusOK || !strings.Contains(requests.Body.String(), `"kind":"request"`) || !strings.Contains(requests.Body.String(), `"path":"/hello"`) || !strings.Contains(requests.Body.String(), `"country":"HR"`) {
		t.Fatalf("unexpected requests: %d %s", requests.Code, requests.Body.String())
	}
	channels := call(t, handler, cookie, session, http.MethodGet, "/api/log/channels?app=sinatra", "")
	if channels.Code != http.StatusOK || !strings.Contains(channels.Body.String(), `"id":"file:production.log"`) {
		t.Fatalf("unexpected channels: %d %s", channels.Code, channels.Body.String())
	}
	tree := call(t, handler, cookie, session, http.MethodGet, "/api/log/tree", "")
	if tree.Code != http.StatusOK || !strings.Contains(tree.Body.String(), `"name":"sinatra"`) || !strings.Contains(tree.Body.String(), `"bytes":4096`) || !strings.Contains(tree.Body.String(), `"id":"file:production.log"`) {
		t.Fatalf("unexpected tree: %d %s", tree.Code, tree.Body.String())
	}
	missingApp := call(t, handler, cookie, session, http.MethodGet, "/api/log/search", "")
	if missingApp.Code != http.StatusBadRequest {
		t.Fatalf("missing app should be a 400: %d", missingApp.Code)
	}
}

func newTestHandler(t *testing.T, manager *fakeManager, rates fakeRates) *Handler {
	t.Helper()
	cfg := config.Default()
	cfg.Apps = "/apps"
	cfg.StateDir = t.TempDir()
	cfg.Management.Host = config.List{"dboss.lvh.me", "dboss.internal"}
	cfg.Management.Admins = []string{"admin@example.com"}
	return handlerFor(t, cfg, manager, rates)
}

// newDevTestHandler is the console of a dev session: one app run from its own folder, with no
// management block at all.
func newDevTestHandler(t *testing.T, manager *fakeManager) *Handler {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.App = &config.App{Procfile: map[string]config.ProcessSpec{"web": {}}}
	return handlerFor(t, cfg, manager, nil)
}

func handlerFor(t *testing.T, cfg config.Config, manager *fakeManager, rates fakeRates) *Handler {
	t.Helper()
	handler, err := New(cfg, authcog.NewWithKey([]byte("01234567890123456789012345678901")), ops.New(manager, fakeLogs{rates: rates}, nil, nil, nil, nil), newFakeStore(), &fakeSys{snapshot: sysinfo.Snapshot{Host: sysinfo.Host{Hostname: "box"}}})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestDevConsoleOpensOnLoopbackWithoutASession(t *testing.T) {
	handler := newDevTestHandler(t, &fakeManager{})
	if !handler.capabilities()["dev"] {
		t.Fatal("a dev console should report the dev capability")
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3100/api/bootstrap", nil)
	request.RemoteAddr = "127.0.0.1:54321"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"viewer":"cli@localhost"`) {
		t.Fatalf("dev bootstrap = %d %s", response.Code, response.Body.String())
	}
}

func TestConsoleServesSystemInspection(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	cookie, session := sessionCookie(t, handler)

	snapshot := call(t, handler, cookie, session, http.MethodGet, "/api/sys", "")
	if snapshot.Code != http.StatusOK || !strings.Contains(snapshot.Body.String(), `"hostname":"box"`) {
		t.Fatalf("unexpected sys snapshot: %d %s", snapshot.Code, snapshot.Body.String())
	}
	refresh := call(t, handler, cookie, session, http.MethodPost, "/api/sys/refresh", "{}")
	if refresh.Code != http.StatusOK || !strings.Contains(refresh.Body.String(), `"hostname":"refreshed"`) {
		t.Fatalf("unexpected sys refresh: %d %s", refresh.Code, refresh.Body.String())
	}
}

// fakeDisk stands in for the diskusage module: it measures one known app and refuses anything else.
type fakeDisk struct{ usage diskusage.Usage }

func (f *fakeDisk) Usage(app string) (diskusage.Usage, bool) {
	if app != "sinatra" {
		return diskusage.Usage{}, false
	}
	return f.usage, true
}

func (f *fakeDisk) Refresh(app string) (diskusage.Usage, error) {
	if app != "sinatra" {
		return diskusage.Usage{}, errors.New("unknown app")
	}
	f.usage = diskusage.Usage{AppBytes: 2048, LogBytes: 512, TotalBytes: 2560, MeasuredAt: time.Now()}
	return f.usage, nil
}

func TestConsoleRefreshesAppDiskUsage(t *testing.T) {
	cfg := config.Default()
	cfg.Apps = "/apps"
	cfg.StateDir = t.TempDir()
	cfg.Management.Host = config.List{"dboss.lvh.me", "dboss.internal"}
	cfg.Management.Admins = []string{"admin@example.com"}
	manager := &fakeManager{snapshots: []supervisor.Snapshot{{Name: "sinatra", State: supervisor.Running}}}
	handler, err := New(cfg, authcog.NewWithKey([]byte("01234567890123456789012345678901")), ops.New(manager, fakeLogs{}, nil, nil, &fakeDisk{}, nil), newFakeStore(), &fakeSys{})
	if err != nil {
		t.Fatal(err)
	}
	cookie, session := sessionCookie(t, handler)

	refresh := call(t, handler, cookie, session, http.MethodPost, "/api/disk/refresh", `{"app":"sinatra"}`)
	if refresh.Code != http.StatusOK || !strings.Contains(refresh.Body.String(), `"total_bytes":2560`) {
		t.Fatalf("disk refresh = %d %s", refresh.Code, refresh.Body.String())
	}
	if missing := call(t, handler, cookie, session, http.MethodPost, "/api/disk/refresh", `{"app":"gone"}`); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown app = %d %s", missing.Code, missing.Body.String())
	}
	if empty := call(t, handler, cookie, session, http.MethodPost, "/api/disk/refresh", `{}`); empty.Code != http.StatusBadRequest {
		t.Fatalf("missing app = %d %s", empty.Code, empty.Body.String())
	}

	// Without the CSRF header the refresh is rejected like every other write.
	request := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/api/disk/refresh", strings.NewReader(`{"app":"sinatra"}`))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code == http.StatusOK {
		t.Fatalf("a refresh without CSRF should be rejected, got %d", response.Code)
	}
}

func TestConsoleServesTraffic(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	cookie, session := sessionCookie(t, handler)

	traffic := call(t, handler, cookie, session, http.MethodGet, "/api/traffic?app=sinatra&range=24h", "")
	if traffic.Code != http.StatusOK || !strings.Contains(traffic.Body.String(), `"count":42`) || !strings.Contains(traffic.Body.String(), `"path":"/hello"`) {
		t.Fatalf("unexpected traffic: %d %s", traffic.Code, traffic.Body.String())
	}
	for _, target := range []string{"/api/traffic?range=24h", "/api/traffic?app=sinatra&range=90d", "/api/traffic?app=sinatra"} {
		if response := call(t, handler, cookie, session, http.MethodGet, target, ""); response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", target, response.Code)
		}
	}
}

func TestConsoleAnswersForEveryManagementHost(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	cookie, _ := sessionCookie(t, handler)
	for host, want := range map[string]int{"dboss.lvh.me:8081": http.StatusOK, "dboss.internal": http.StatusOK, "other.lvh.me:8081": http.StatusNotFound} {
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
	request := httptest.NewRequest(method, "http://dboss.lvh.me:8081"+target, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Origin", "http://dboss.lvh.me:8081")
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
	var result ops.ConfigResult
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

	noCSRF := httptest.NewRequest(http.MethodPut, "http://dboss.lvh.me:8081/api/config/file", strings.NewReader(`{"id":"host","contents":"","revision":""}`))
	noCSRF.Header.Set("Content-Type", "application/json")
	noCSRF.AddCookie(cookie)
	noCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(noCSRFResponse, noCSRF)
	if noCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("write without CSRF token: %d", noCSRFResponse.Code)
	}
}

func TestConsoleConfigFormRoundTrip(t *testing.T) {
	manager := &fakeManager{}
	handler := newTestHandler(t, manager, nil)
	store := handler.store.(*fakeStore)
	cookie, session := sessionCookie(t, handler)

	form := call(t, handler, cookie, session, http.MethodGet, "/api/config/form?id=app:sinatra", "")
	if form.Code != http.StatusOK {
		t.Fatalf("form: %d %s", form.Code, form.Body.String())
	}
	var payload struct {
		Values  map[string]any `json:"values"`
		Recipes []struct {
			ID     string `json:"id"`
			Scope  string `json:"scope"`
			Fields []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"fields"`
		} `json:"recipes"`
	}
	if err := json.Unmarshal(form.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Recipes) == 0 {
		t.Fatal("no app recipes returned")
	}
	for _, recipe := range payload.Recipes {
		if recipe.Scope != "app" {
			t.Errorf("host recipe %q leaked into an app form", recipe.ID)
		}
	}

	file, err := store.Read("app:sinatra")
	if err != nil {
		t.Fatal(err)
	}
	applyBody := `{"id":"app:sinatra","revision":"` + file.Revision + `","recipe":"web","values":{"max_body":"20m"},"reset":["deletable"]}`
	applied := call(t, handler, cookie, session, http.MethodPost, "/api/config/apply", applyBody)
	if applied.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", applied.Code, applied.Body.String())
	}
	if !store.files["app:sinatra"].HasLocal {
		t.Error("apply did not create the server override")
	}
	contents := store.files["app:sinatra"].Contents
	if !strings.Contains(contents, "20m") {
		t.Errorf("apply did not write the recipe values:\n%s", contents)
	}
	if manager.actions[len(manager.actions)-1] != "rescan" {
		t.Errorf("apply did not rescan: %v", manager.actions)
	}

	// A key outside the recipe is ignored rather than written.
	current, err := store.Read("app:sinatra")
	if err != nil {
		t.Fatal(err)
	}
	foreign := `{"id":"app:sinatra","revision":"` + current.Revision + `","recipe":"web","values":{"log_retention":"999h"},"reset":[]}`
	if got := call(t, handler, cookie, session, http.MethodPost, "/api/config/apply", foreign); got.Code != http.StatusOK {
		t.Fatalf("foreign apply: %d %s", got.Code, got.Body.String())
	}
	if strings.Contains(store.files["app:sinatra"].Contents, "999h") {
		t.Error("a key outside the recipe was written")
	}

	unknown := `{"id":"app:sinatra","revision":"` + current.Revision + `","recipe":"nope","values":{},"reset":[]}`
	if got := call(t, handler, cookie, session, http.MethodPost, "/api/config/apply", unknown); got.Code != http.StatusBadRequest {
		t.Errorf("unknown recipe should be a 400: %d %s", got.Code, got.Body.String())
	}
}

// TestConsoleConfigFormWritesRealOverride drives the form endpoints through the real
// apps.Store and checks that a save lands in the server-only override on disk.
func TestConsoleConfigFormWritesRealOverride(t *testing.T) {
	root := t.TempDir()
	hostDir := filepath.Join(root, "host")
	appDir := filepath.Join(hostDir, "apps", "sinatra")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hostPath := filepath.Join(hostDir, config.FileName)
	if err := os.WriteFile(hostPath, []byte("apps: ./apps\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(appDir, config.FileName)
	if err := os.WriteFile(appPath, []byte("procfile:\n  web: ./server\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.SourcePath = hostPath
	cfg.Dir = hostDir
	cfg.Apps = filepath.Join(hostDir, "apps")
	cfg.StateDir = filepath.Join(root, "state")
	cfg.LogDir = filepath.Join(root, "log")
	cfg.Management.Host = config.List{"dboss.lvh.me"}
	cfg.Management.Admins = []string{"admin@example.com"}
	store := apps.NewStore(cfg)
	manager := &fakeManager{}
	handler, err := New(cfg, authcog.NewWithKey([]byte("01234567890123456789012345678901")), ops.New(manager, fakeLogs{}, nil, nil, nil, nil), store, &fakeSys{})
	if err != nil {
		t.Fatal(err)
	}
	cookie, session := sessionCookie(t, handler)

	form := call(t, handler, cookie, session, http.MethodGet, "/api/config/form?id=app:sinatra", "")
	if form.Code != http.StatusOK {
		t.Fatalf("form: %d %s", form.Code, form.Body.String())
	}
	var payload struct {
		File    apps.ConfigFile `json:"file"`
		Recipes []config.Recipe `json:"recipes"`
	}
	if err := json.Unmarshal(form.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, recipe := range payload.Recipes {
		if recipe.Scope != config.RecipeApp {
			t.Errorf("host recipe %q in an app form", recipe.ID)
		}
	}

	apply := `{"id":"app:sinatra","revision":"` + payload.File.Revision + `","recipe":"web","values":{"max_body":"20m"},"reset":[]}`
	applied := call(t, handler, cookie, session, http.MethodPost, "/api/config/apply", apply)
	if applied.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", applied.Code, applied.Body.String())
	}
	override, err := os.ReadFile(filepath.Join(appDir, config.LocalFileName))
	if err != nil {
		t.Fatalf("override was not written: %v", err)
	}
	if !strings.Contains(string(override), "20m") {
		t.Fatalf("override is missing the values:\n%s", override)
	}
	base, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(base), "20m") {
		t.Fatal("the base dboss.yaml was modified")
	}
	if manager.actions[len(manager.actions)-1] != "rescan" {
		t.Fatalf("apply did not rescan: %v", manager.actions)
	}

	hostForm := call(t, handler, cookie, session, http.MethodGet, "/api/config/form?id=host", "")
	var hostPayload struct {
		File    apps.ConfigFile `json:"file"`
		Recipes []config.Recipe `json:"recipes"`
	}
	if err := json.Unmarshal(hostForm.Body.Bytes(), &hostPayload); err != nil {
		t.Fatal(err)
	}
	sawNotifications := false
	for _, recipe := range hostPayload.Recipes {
		if recipe.Scope != config.RecipeHost {
			t.Errorf("app recipe %q in a host form", recipe.ID)
		}
		if recipe.ID == "notifications" {
			sawNotifications = true
		}
	}
	if !sawNotifications {
		t.Fatal("the host form has no notifications recipe")
	}
	hostApply := `{"id":"host","revision":"` + hostPayload.File.Revision + `","recipe":"notifications","values":{"notify.url":"https://hooks.example.com"},"reset":[]}`
	if got := call(t, handler, cookie, session, http.MethodPost, "/api/config/apply", hostApply); got.Code != http.StatusOK {
		t.Fatalf("host apply: %d %s", got.Code, got.Body.String())
	}
	hostOverride, err := os.ReadFile(filepath.Join(hostDir, config.LocalFileName))
	if err != nil {
		t.Fatalf("host override was not written: %v", err)
	}
	if !strings.Contains(string(hostOverride), "hooks.example.com") {
		t.Fatalf("host override is missing the webhook URL:\n%s", hostOverride)
	}
}

func sessionCookie(t *testing.T, handler *Handler) (*http.Cookie, authSession) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/", nil)
	response := httptest.NewRecorder()
	if err := handler.auth.setSessionCookie(response, request, "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	cookie := cookieNamed(t, response.Result().Cookies(), authSessionCookie)
	authorized := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/", nil)
	authorized.AddCookie(cookie)
	session, ok := handler.auth.validSession(authorized)
	if !ok || session.ExpiresAt <= time.Now().Unix() {
		t.Fatal("generated session is invalid")
	}
	return cookie, session
}
