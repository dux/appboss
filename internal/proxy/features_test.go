package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"dboss/internal/config"
	"dboss/internal/logstore"
	"dboss/internal/supervisor"
)

// fakeRecorder captures the deny counter a blocked request reports.
type fakeRecorder struct{ blocked []string }

func (f *fakeRecorder) Record(string, time.Duration, logstore.RequestEntry) error { return nil }

func (f *fakeRecorder) RecordBlocked(path string) error {
	f.blocked = append(f.blocked, path)
	return nil
}

// featureHandler has no manager: every step before forwarding must answer on its own.
func featureHandler() *Handler {
	handler := &Handler{cfg: config.Default(), hostConfig: config.Default}
	handler.initFilters()
	return handler
}

func featureSnapshot(t *testing.T, data string) supervisor.Snapshot {
	t.Helper()
	dir := t.TempDir()
	app, err := config.ParseApp([]byte("procfile:\n  web:\n    command: ./server\n    hosts: [demo.test, www.demo.test]\n"+data), filepath.Join(dir, config.FileName), config.Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	return supervisor.Snapshot{Name: "demo", State: supervisor.Running, Dir: dir, Pages: filepath.Join(dir, app.Pages), Hosts: app.Hosts, WebProcesses: supervisor.WebProcessSnapshots(app.WebProcesses), Web: app.Web}
}

func serveFeature(t *testing.T, handler *Handler, snapshot supervisor.Snapshot, request *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.serve(&responseRecorder{ResponseWriter: response, status: http.StatusOK}, request, snapshot)
	return response
}

func TestDrainingAppAnswers503(t *testing.T) {
	snapshot := featureSnapshot(t, "")
	snapshot.Draining = true
	request := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	request.Host = "demo.test"
	request.Header.Set("Accept", "text/html")
	response := serveFeature(t, featureHandler(), snapshot, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining status = %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("draining response is missing Retry-After")
	}
}

func TestHealthEndpointReportsState(t *testing.T) {
	path := "/.well-known/dboss/health"
	get := func(snapshot supervisor.Snapshot) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "http://demo.test"+path, nil)
		request.Host = "demo.test"
		return serveFeature(t, featureHandler(), snapshot, request)
	}
	running := featureSnapshot(t, "")
	running.Web.BasicAuth = map[string]string{"alice": "$2a$10$doesnotmatter"}
	response := get(running)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"running"`) {
		t.Fatalf("running health = %d %s", response.Code, response.Body.String())
	}
	// A sleeping app wakes on the next request, so a health check must not report it down.
	stopped := featureSnapshot(t, "")
	stopped.State = supervisor.Stopped
	if response := get(stopped); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"stopped"`) {
		t.Fatalf("stopped health = %d %s", response.Code, response.Body.String())
	}
	button := featureSnapshot(t, "")
	button.State = supervisor.Stopped
	button.WakeButton = true
	if response := get(button); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("stopped button health = %d", response.Code)
	}
	crashed := featureSnapshot(t, "")
	crashed.State = supervisor.Crashed
	if response := get(crashed); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("crashed health = %d", response.Code)
	}
	maintenance := featureSnapshot(t, "")
	maintenance.Maintenance = true
	if response := get(maintenance); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("maintenance health = %d", response.Code)
	}
	draining := featureSnapshot(t, "")
	draining.Draining = true
	if response := get(draining); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining health = %d", response.Code)
	}
}

func TestCanonicalHostRedirect(t *testing.T) {
	snapshot := featureSnapshot(t, "    canonical_host: demo.test\n")
	// A plain-HTTP origin, which is every local session and any box not behind Cloudflare, has
	// to be sent back to http; assuming https pointed the browser at a port nothing listens on.
	request := httptest.NewRequest(http.MethodGet, "http://www.demo.test:8080/path?x=1", nil)
	request.Host = "WWW.demo.test:8080"
	response := serveFeature(t, featureHandler(), snapshot, request)
	if response.Code != http.StatusMovedPermanently || response.Header().Get("Location") != "http://demo.test/path?x=1" {
		t.Fatalf("unexpected redirect: %d %s", response.Code, response.Header().Get("Location"))
	}
	// Cloudflare's header wins, so an origin reached over plain HTTP still redirects to https.
	forwarded := httptest.NewRequest(http.MethodGet, "http://www.demo.test:8080/path?x=1", nil)
	forwarded.Host = "www.demo.test:8080"
	forwarded.Header.Set("X-Forwarded-Proto", "https")
	if response := serveFeature(t, featureHandler(), snapshot, forwarded); response.Header().Get("Location") != "https://demo.test/path?x=1" {
		t.Fatalf("scheme should follow X-Forwarded-Proto: %s", response.Header().Get("Location"))
	}
	// So does dboss terminating TLS itself, with no header in play.
	secure := httptest.NewRequest(http.MethodGet, "https://www.demo.test/path?x=1", nil)
	secure.Host = "www.demo.test"
	if response := serveFeature(t, featureHandler(), snapshot, secure); response.Header().Get("Location") != "https://demo.test/path?x=1" {
		t.Fatalf("own TLS should redirect to https: %s", response.Header().Get("Location"))
	}
	request.Host = "demo.test"
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Code == http.StatusMovedPermanently {
		t.Fatal("canonical host must not redirect")
	}
}

func TestCanonicalHostPerWebProcess(t *testing.T) {
	dir := t.TempDir()
	app, err := config.ParseApp([]byte("procfile:\n  shop:\n    command: ./shop\n    hosts: [shop.test, www.shop.test]\n    canonical_host: shop.test\n  admin:\n    command: ./admin\n    hosts: [admin.test, www.admin.test]\n    canonical_host: admin.test\n"), filepath.Join(dir, config.FileName), config.Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := supervisor.Snapshot{Name: "demo", State: supervisor.Running, Dir: dir, Hosts: app.Hosts, WebProcesses: supervisor.WebProcessSnapshots(app.WebProcesses), Web: app.Web}
	// The scheme mirrors the plain-HTTP requests below; what matters here is that each web
	// process redirects to its own canonical host, not the other one's.
	for host, want := range map[string]string{"www.shop.test": "http://shop.test/", "www.admin.test": "http://admin.test/"} {
		request := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		request.Host = host
		response := serveFeature(t, featureHandler(), snapshot, request)
		if response.Code != http.StatusMovedPermanently || response.Header().Get("Location") != want {
			t.Errorf("%s: got %d %s, want %s", host, response.Code, response.Header().Get("Location"), want)
		}
	}
}

func TestAllowIPsUsesClientIPHeader(t *testing.T) {
	handler := featureHandler()
	handler.cfg.Proxy.Cloudflare = true
	snapshot := featureSnapshot(t, "allow_ips: [10.0.0.0/8]\n")
	request := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	request.Header.Set("Accept", "text/html")
	request.Header.Set("CF-Connecting-IP", "203.0.113.1")
	response := serveFeature(t, handler, snapshot, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "Access denied") {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
	request.Header.Set("CF-Connecting-IP", "10.1.1.1")
	if response := serveFeature(t, handler, snapshot, request); response.Code == http.StatusForbidden {
		t.Fatal("allowed address was rejected")
	}
	request.Header.Set("CF-Connecting-IP", "203.0.113.1")
	request.Header.Del("Accept")
	if response := serveFeature(t, handler, snapshot, request); response.Code != http.StatusForbidden || response.Body.Len() != 0 {
		t.Fatalf("non-html client should get an empty 403: %d %q", response.Code, response.Body.String())
	}
}

func TestDenyBlocksPaths(t *testing.T) {
	snapshot := featureSnapshot(t, "deny: [\"*.php\", /admin/*, /server-status]\n")
	get := func(path string, html bool) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "http://demo.test"+path, nil)
		request.Host = "demo.test"
		if html {
			request.Header.Set("Accept", "text/html")
		}
		return serveFeature(t, featureHandler(), snapshot, request)
	}
	for _, path := range []string{"/index.php", "/deep/nested/shell.PHP", "/admin", "/admin/users", "/server-status"} {
		response := get(path, true)
		if response.Code != http.StatusForbidden {
			t.Errorf("%s should be blocked, got %d", path, response.Code)
		}
	}
	if response := get("/index.php", true); !strings.Contains(response.Body.String(), "This page is not available") {
		t.Errorf("blocked page body = %q", response.Body.String())
	}
	if response := get("/index.php", false); response.Code != http.StatusForbidden || response.Body.Len() != 0 {
		t.Errorf("non-html client should get an empty 403: %d %q", response.Code, response.Body.String())
	}
	for _, path := range []string{"/index.html", "/server-status-page", "/administrator"} {
		if response := get(path, true); response.Code == http.StatusForbidden {
			t.Errorf("%s should not be blocked", path)
		}
	}
}

func TestDenyRecordsBlockedPath(t *testing.T) {
	handler := featureHandler()
	recorder := &fakeRecorder{}
	handler.recorder = recorder
	snapshot := featureSnapshot(t, "deny: [\"*.php\"]\n")
	request := httptest.NewRequest(http.MethodGet, "http://demo.test/deep/x.php?q=1", nil)
	request.Host = "demo.test"
	if response := serveFeature(t, handler, snapshot, request); response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
	if len(recorder.blocked) != 1 || recorder.blocked[0] != "/deep/x.php" {
		t.Fatalf("blocked = %v", recorder.blocked)
	}
}

func TestBasicAuthProtectsStaticToo(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := featureSnapshot(t, "static: ./public\nbasic_auth:\n  alice: \""+string(hash)+"\"\n")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", "robots.txt"), "User-agent: *\n")
	request := httptest.NewRequest(http.MethodGet, "http://demo.test/robots.txt", nil)
	response := serveFeature(t, featureHandler(), snapshot, request)
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != `Basic realm="demo"` {
		t.Fatalf("unexpected challenge: %d %q", response.Code, response.Header().Get("WWW-Authenticate"))
	}
	request.SetBasicAuth("alice", "wrong")
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password accepted: %d", response.Code)
	}
	request.SetBasicAuth("alice", "secret")
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Code != http.StatusOK || response.Body.String() != "User-agent: *\n" {
		t.Fatalf("unexpected static response: %d %q", response.Code, response.Body.String())
	}
}

func TestBasicAuthAcceptsPlainPassword(t *testing.T) {
	snapshot := featureSnapshot(t, "basic_auth:\n  alice: secret\n")
	request := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	request.SetBasicAuth("alice", "secre")
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password accepted: %d", response.Code)
	}
	request.SetBasicAuth("alice", "secret")
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Code == http.StatusUnauthorized {
		t.Fatalf("plain password rejected: %d", response.Code)
	}
}

// A request that fails basic auth must stop at the auth stage. The feature handler has no manager,
// so reaching the wake stage for this stopped app would panic instead of answering 401.
func TestUnauthorizedRequestNeverWakesAStoppedApp(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := featureSnapshot(t, "basic_auth:\n  alice: \""+string(hash)+"\"\n")
	snapshot.State = supervisor.Stopped
	request := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	request.Header.Set("Accept", "text/html")
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials on a stopped app = %d", response.Code)
	}
	request.SetBasicAuth("alice", "wrong")
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password on a stopped app = %d", response.Code)
	}
	// The health endpoint stays open in front of auth and reports the sleeping app as serving.
	health := httptest.NewRequest(http.MethodGet, "http://demo.test/.well-known/dboss/health", nil)
	if response := serveFeature(t, featureHandler(), snapshot, health); response.Code != http.StatusOK {
		t.Fatalf("health on a protected stopped app = %d", response.Code)
	}
}

func TestMaintenancePageLookup(t *testing.T) {
	snapshot := featureSnapshot(t, "")
	snapshot.Maintenance = true
	request := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	request.Header.Set("Accept", "text/html")
	response := serveFeature(t, featureHandler(), snapshot, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "30" || !strings.Contains(response.Body.String(), "demo is under maintenance") {
		t.Fatalf("unexpected built-in page: %d %s", response.Code, response.Body.String())
	}
	writeProxyFixture(t, filepath.Join(snapshot.Pages, "template.html"), "<h1>{{status}} {{title}}</h1>")
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Body.String() != "<h1>503 demo is under maintenance</h1>" {
		t.Fatalf("the app template should win over the built-in page: %s", response.Body.String())
	}
	writeProxyFixture(t, filepath.Join(snapshot.Pages, "maintenance.html"), "<h1>down {{app}}</h1>")
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Body.String() != "<h1>down demo</h1>" {
		t.Fatalf("maintenance.html should win over the template: %s", response.Body.String())
	}
	post := httptest.NewRequest(http.MethodPost, "http://demo.test/", nil)
	if response := serveFeature(t, featureHandler(), snapshot, post); response.Code != http.StatusServiceUnavailable || response.Body.Len() != 0 {
		t.Fatalf("non-html request should get an empty 503: %d %q", response.Code, response.Body.String())
	}
}

func TestHostPagesAreTheFallback(t *testing.T) {
	host := t.TempDir()
	writeProxyFixture(t, filepath.Join(host, "template.html"), "<p>host {{title}}</p>")
	handler := featureHandler()
	handler.hostConfig = func() config.Config {
		cfg := config.Default()
		cfg.Pages = host
		return cfg
	}
	snapshot := featureSnapshot(t, "")
	snapshot.Maintenance = true
	request := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	request.Header.Set("Accept", "text/html")
	if response := serveFeature(t, handler, snapshot, request); response.Body.String() != "<p>host demo is under maintenance</p>" {
		t.Fatalf("host template should serve an app without pages: %s", response.Body.String())
	}
	writeProxyFixture(t, filepath.Join(snapshot.Pages, "template.html"), "<p>app</p>")
	if response := serveFeature(t, handler, snapshot, request); response.Body.String() != "<p>app</p>" {
		t.Fatalf("app pages should win over the host: %s", response.Body.String())
	}
}

func TestStaticFilesStayInsideRoot(t *testing.T) {
	snapshot := featureSnapshot(t, "static: ./public\nheaders:\n  X-Robots-Tag: none\n")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "secret.txt"), "secret")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", "assets", "app.css"), "body{}")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", "index.html"), "<p>index</p>")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", "robots.txt"), "User-agent: *")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", ".env"), "SECRET=1")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", "LICENSE"), "MIT")
	if err := os.Symlink(filepath.Join(snapshot.Dir, "secret.txt"), filepath.Join(snapshot.Dir, "public", "link.txt")); err != nil {
		t.Fatal(err)
	}
	get := func(target string) *httptest.ResponseRecorder {
		return serveFeature(t, featureHandler(), snapshot, httptest.NewRequest(http.MethodGet, "http://demo.test"+target, nil))
	}
	response := get("/assets/app.css")
	if response.Code != http.StatusOK || response.Body.String() != "body{}" || response.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || response.Header().Get("X-Robots-Tag") != "none" || !strings.Contains(response.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("unexpected asset response: %d %v %s", response.Code, response.Header(), response.Body.String())
	}
	if response := get("/robots.txt"); response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "public, max-age=3600" {
		t.Fatalf("unexpected robots.txt response: %d %q", response.Code, response.Header().Get("Cache-Control"))
	}
	// Anything the static step does not answer reaches forward, which has no port here and answers 502.
	// A file outside static_extensions (a page, a dotfile, no extension) is the app's, even when it exists.
	for _, target := range []string{"/assets/../../secret.txt", "/link.txt", "/assets/", "/", "/missing.txt", "/index.html", "/.env", "/LICENSE"} {
		if response := get(target); response.Code != http.StatusBadGateway {
			t.Fatalf("%s should fall through to the app, got %d %q", target, response.Code, response.Body.String())
		}
	}
}

// public/ is served without any config, and only for the common asset extensions.
func TestStaticDefaultsToPublicDirectory(t *testing.T) {
	snapshot := featureSnapshot(t, "")
	if web, ok := snapshot.WebForHost("demo.test"); !ok || web.Static != "./public" {
		t.Fatalf("default static = %+v", web)
	}
	get := func(target string) *httptest.ResponseRecorder {
		return serveFeature(t, featureHandler(), snapshot, httptest.NewRequest(http.MethodGet, "http://demo.test"+target, nil))
	}
	// No public directory yet: static serving is simply off.
	if response := get("/logo.png"); response.Code != http.StatusBadGateway {
		t.Fatalf("missing public dir should reach the app, got %d", response.Code)
	}
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", "logo.PNG"), "png")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", "foo", "bar.png"), "nested")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", "about.html"), "<p>about</p>")
	if response := get("/logo.PNG"); response.Code != http.StatusOK || response.Body.String() != "png" {
		t.Fatalf("asset from default public dir: %d %q", response.Code, response.Body.String())
	}
	if response := get("/foo/bar.png"); response.Code != http.StatusOK || response.Body.String() != "nested" {
		t.Fatalf("nested asset keeps its path: %d %q", response.Code, response.Body.String())
	}
	if response := get("/about.html"); response.Code != http.StatusBadGateway {
		t.Fatalf("html under public should reach the app, got %d", response.Code)
	}
	post := serveFeature(t, featureHandler(), snapshot, httptest.NewRequest(http.MethodPost, "http://demo.test/logo.PNG", nil))
	if post.Code != http.StatusBadGateway {
		t.Fatalf("POST should reach the app, got %d", post.Code)
	}
}

func TestStaticExtensionsEmptyServesAnyFile(t *testing.T) {
	snapshot := featureSnapshot(t, "static_extensions: []\n")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", "about.html"), "<p>about</p>")
	response := serveFeature(t, featureHandler(), snapshot, httptest.NewRequest(http.MethodGet, "http://demo.test/about.html", nil))
	if response.Code != http.StatusOK || response.Body.String() != "<p>about</p>" {
		t.Fatalf("empty static_extensions should serve any file: %d %q", response.Code, response.Body.String())
	}
	custom := featureSnapshot(t, "static_extensions: [html]\n")
	writeProxyFixture(t, filepath.Join(custom.Dir, "public", "about.html"), "<p>about</p>")
	writeProxyFixture(t, filepath.Join(custom.Dir, "public", "app.css"), "body{}")
	if response := serveFeature(t, featureHandler(), custom, httptest.NewRequest(http.MethodGet, "http://demo.test/about.html", nil)); response.Code != http.StatusOK {
		t.Fatalf("listed extension: %d", response.Code)
	}
	if response := serveFeature(t, featureHandler(), custom, httptest.NewRequest(http.MethodGet, "http://demo.test/app.css", nil)); response.Code != http.StatusBadGateway {
		t.Fatalf("unlisted extension should reach the app, got %d", response.Code)
	}
}

// The feature handler has no upstream, so every forwarded request is a proxy error.
func TestErrorPageForProxyErrors(t *testing.T) {
	get := func(snapshot supervisor.Snapshot, accept string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
		if accept != "" {
			request.Header.Set("Accept", accept)
		}
		return serveFeature(t, featureHandler(), snapshot, request)
	}
	builtIn := get(featureSnapshot(t, ""), "text/html")
	if builtIn.Code != http.StatusBadGateway || !strings.Contains(builtIn.Body.String(), "Something went wrong") || builtIn.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("built-in error page = %d %v %q", builtIn.Code, builtIn.Header(), builtIn.Body.String())
	}
	if api := get(featureSnapshot(t, ""), "application/json"); api.Code != http.StatusBadGateway || api.Body.Len() != 0 {
		t.Fatalf("non-html client should get an empty 502: %d %q", api.Code, api.Body.String())
	}

	custom := featureSnapshot(t, "")
	writeProxyFixture(t, filepath.Join(custom.Pages, "error.html"), "<h1>ours {{status}} {{app}} {{unknown}}</h1>")
	response := get(custom, "text/html")
	if response.Code != http.StatusBadGateway || response.Body.String() != "<h1>ours 502 demo {{unknown}}</h1>" || !strings.Contains(response.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("custom error page = %d %v %q", response.Code, response.Header(), response.Body.String())
	}
	if api := get(custom, ""); api.Code != http.StatusBadGateway || api.Body.Len() != 0 {
		t.Fatalf("non-html client should get an empty 502 with a custom page too: %d %q", api.Code, api.Body.String())
	}
}

func TestMaxBodyRejectsDeclaredLength(t *testing.T) {
	snapshot := featureSnapshot(t, "max_body: 1k\n")
	request := httptest.NewRequest(http.MethodPost, "http://demo.test/upload", strings.NewReader(strings.Repeat("x", 2048)))
	response := serveFeature(t, featureHandler(), snapshot, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("unexpected status: %d", response.Code)
	}
}

func TestApplyHeadersRemovesEmptyValues(t *testing.T) {
	header := http.Header{"X-Powered-By": []string{"sinatra"}, "Server": []string{"puma"}}
	applyHeaders(header, map[string]string{"X-Powered-By": "", "X-Frame-Options": "DENY"})
	if _, ok := header["X-Powered-By"]; ok || header.Get("X-Frame-Options") != "DENY" || header.Get("Server") != "puma" {
		t.Fatalf("unexpected headers: %v", header)
	}
}
