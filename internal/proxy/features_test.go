package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"dboss/internal/config"
	"dboss/internal/super"
)

// featureHandler has no manager: every step before forwarding must answer on its own.
func featureHandler() *Handler {
	handler := &Handler{cfg: config.Default(), button: []byte(defaultButtonPage), maintenance: []byte(defaultMaintenancePage)}
	handler.initFilters()
	return handler
}

func featureSnapshot(t *testing.T, data string) super.Snapshot {
	t.Helper()
	dir := t.TempDir()
	app, err := config.ParseApp([]byte("procfile:\n  web: ./server\nhosts: [demo.test, www.demo.test]\n"+data), filepath.Join(dir, config.FileName), config.Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	return super.Snapshot{Name: "demo", State: super.Running, Dir: dir, Hosts: app.Hosts, CanonicalHost: app.CanonicalHost, WebProcess: app.WebProcess, Web: app.Web}
}

func serveFeature(t *testing.T, handler *Handler, snapshot super.Snapshot, request *http.Request) *httptest.ResponseRecorder {
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
	get := func(snapshot super.Snapshot) *httptest.ResponseRecorder {
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
	stopped := featureSnapshot(t, "")
	stopped.State = super.Stopped
	if response := get(stopped); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("stopped health = %d", response.Code)
	}
	draining := featureSnapshot(t, "")
	draining.Draining = true
	if response := get(draining); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining health = %d", response.Code)
	}
}

func TestCanonicalHostRedirect(t *testing.T) {
	snapshot := featureSnapshot(t, "canonical_host: demo.test\n")
	request := httptest.NewRequest(http.MethodGet, "http://www.demo.test:8080/path?x=1", nil)
	request.Host = "WWW.demo.test:8080"
	response := serveFeature(t, featureHandler(), snapshot, request)
	if response.Code != http.StatusMovedPermanently || response.Header().Get("Location") != "https://demo.test/path?x=1" {
		t.Fatalf("unexpected redirect: %d %s", response.Code, response.Header().Get("Location"))
	}
	request.Header.Set("X-Forwarded-Proto", "http")
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Header().Get("Location") != "http://demo.test/path?x=1" {
		t.Fatalf("scheme should follow X-Forwarded-Proto: %s", response.Header().Get("Location"))
	}
	request.Host = "demo.test"
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Code == http.StatusMovedPermanently {
		t.Fatal("canonical host must not redirect")
	}
}

func TestAllowIPsUsesClientIPHeader(t *testing.T) {
	snapshot := featureSnapshot(t, "allow_ips: [10.0.0.0/8]\n")
	request := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	request.Header.Set("Accept", "text/html")
	request.Header.Set("CF-Connecting-IP", "203.0.113.1")
	response := serveFeature(t, featureHandler(), snapshot, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "Forbidden") {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
	request.Header.Set("CF-Connecting-IP", "10.1.1.1")
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Code == http.StatusForbidden {
		t.Fatal("allowed address was rejected")
	}
	request.Header.Set("CF-Connecting-IP", "203.0.113.1")
	request.Header.Del("Accept")
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Code != http.StatusForbidden || response.Body.Len() != 0 {
		t.Fatalf("non-html client should get an empty 403: %d %q", response.Code, response.Body.String())
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

func TestMaintenancePageLookup(t *testing.T) {
	snapshot := featureSnapshot(t, "static: ./public\n")
	snapshot.Maintenance = true
	request := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	request.Header.Set("Accept", "text/html")
	response := serveFeature(t, featureHandler(), snapshot, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "30" || !strings.Contains(response.Body.String(), "demo will be back shortly") {
		t.Fatalf("unexpected built-in page: %d %s", response.Code, response.Body.String())
	}
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", "503.html"), "<h1>static 503</h1>")
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Body.String() != "<h1>static 503</h1>" {
		t.Fatalf("static/503.html should win over the built-in page: %s", response.Body.String())
	}
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "down.html"), "<h1>custom</h1>")
	snapshot.Web.MaintenancePage = "down.html"
	if response := serveFeature(t, featureHandler(), snapshot, request); response.Body.String() != "<h1>custom</h1>" {
		t.Fatalf("maintenance_page should win: %s", response.Body.String())
	}
	post := httptest.NewRequest(http.MethodPost, "http://demo.test/", nil)
	if response := serveFeature(t, featureHandler(), snapshot, post); response.Code != http.StatusServiceUnavailable || response.Body.Len() != 0 {
		t.Fatalf("non-html request should get an empty 503: %d %q", response.Code, response.Body.String())
	}
}

func TestStaticFilesStayInsideRoot(t *testing.T) {
	snapshot := featureSnapshot(t, "static: ./public\nheaders:\n  X-Robots-Tag: none\n")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "secret.txt"), "secret")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", "assets", "app.css"), "body{}")
	writeProxyFixture(t, filepath.Join(snapshot.Dir, "public", "index.html"), "<p>index</p>")
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
	if response := get("/index.html"); response.Header().Get("Cache-Control") != "public, max-age=3600" {
		t.Fatalf("unexpected cache header: %q", response.Header().Get("Cache-Control"))
	}
	// Anything the static step does not answer reaches forward, which has no port here and answers 502.
	for _, target := range []string{"/assets/../../secret.txt", "/link.txt", "/assets/", "/", "/missing.txt"} {
		if response := get(target); response.Code != http.StatusBadGateway {
			t.Fatalf("%s should fall through to the app, got %d %q", target, response.Code, response.Body.String())
		}
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
