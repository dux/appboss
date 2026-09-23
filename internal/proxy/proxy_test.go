package proxy

import (
	"bufio"
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dboss/internal/authcog"
	"dboss/internal/config"
	"dboss/internal/logstore"
	"dboss/internal/ports"
	"dboss/internal/supervisor"
)

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "http://example.test", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")
	if got := clientIP(r, true); got != "203.0.113.9" {
		t.Fatalf("got %q", got)
	}
}

func TestBufferRequestKeepsSmallBodyInMemory(t *testing.T) {
	payload := []byte("hello")
	request := httptest.NewRequest(http.MethodPost, "http://demo.test/upload", bytes.NewReader(payload))
	var body io.ReadCloser
	bufferRequest(httptest.NewRecorder(), request, 0, func() { body = request.Body })
	if _, ok := body.(*os.File); ok {
		t.Fatal("small body should stay in memory")
	}
	if request.ContentLength != int64(len(payload)) {
		t.Fatalf("content length = %d", request.ContentLength)
	}
	got, _ := io.ReadAll(body)
	if !bytes.Equal(got, payload) {
		t.Fatalf("buffered body = %q", got)
	}
}

func TestBufferRequestSpillsLargeBodyThenRemovesTempFile(t *testing.T) {
	payload := bytes.Repeat([]byte("abcd"), 400*1024)
	request := httptest.NewRequest(http.MethodPost, "http://demo.test/upload", bytes.NewReader(payload))
	request.ContentLength = -1
	var got []byte
	var temp string
	bufferRequest(httptest.NewRecorder(), request, 0, func() {
		got, _ = io.ReadAll(request.Body)
		if file, ok := request.Body.(*os.File); ok {
			temp = file.Name()
		}
	})
	if !bytes.Equal(got, payload) {
		t.Fatalf("buffered body mismatch: got %d bytes want %d", len(got), len(payload))
	}
	if temp == "" {
		t.Fatal("large body was not spilled to a temp file")
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("temp file %s should be removed: %v", temp, err)
	}
}

func TestBufferRequestRejectsOversizeWithoutForwarding(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://demo.test/upload", strings.NewReader(strings.Repeat("x", 4096)))
	response := httptest.NewRecorder()
	called := false
	bufferRequest(response, request, 1024, func() { called = true })
	if response.Code != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("oversize body: status %d forwarded %v", response.Code, called)
	}
}

func TestWakeProxyAndRequestLog(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "demo")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	appConfig := fmt.Sprintf("procfile:\n  web:\n    command: %s -test.run=TestProxyHelperProcess\n    hosts: [demo.test]\nmax_body: 1k\nheaders:\n  X-Powered-By: \"\"\n  X-Frame-Options: DENY\n", os.Args[0])
	writeProxyFixture(t, filepath.Join(appDir, config.FileName), appConfig)
	writeProxyFixture(t, filepath.Join(appDir, ".env"), "BOSS_PROXY_HELPER=1\n")
	writeProxyFixture(t, filepath.Join(appDir, "public", "error_pages", "error.html"), "<h1>custom error {{app}} {{status}}</h1>")
	cfg := config.Default()
	cfg.Apps = filepath.Join(root, "apps")
	cfg.StateDir = filepath.Join(root, "state")
	cfg.LogDir = filepath.Join(root, "log")
	cfg.Socket = filepath.Join(root, "dboss.sock")
	cfg.Ports = [2]int{32200, 32220}
	cfg.Proxy.Cloudflare = true
	cfg.Defaults.HealthTimeout = config.Duration(2 * time.Second)
	manager, invalid, err := supervisor.New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.Boot()
	defer manager.Close()
	if len(invalid) != 0 {
		t.Fatalf("invalid apps: %v", invalid)
	}
	requestLogs := logstore.New(cfg.LogDir, 10*time.Millisecond, nil, "", time.Hour, 0)
	defer requestLogs.Close()
	handler, err := New(cfg, authcog.NewWithKey([]byte("01234567890123456789012345678901")), manager, requestLogs, nil)
	if err != nil {
		t.Fatal(err)
	}
	wakeRequest := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	wakeRequest.Host = "demo.test"
	wakeRequest.Header.Set("Accept", "text/html")
	wakeResponse := httptest.NewRecorder()
	handler.ServeHTTP(wakeResponse, wakeRequest)
	if wakeResponse.Code != http.StatusServiceUnavailable || !strings.Contains(wakeResponse.Body.String(), "demo is starting") {
		t.Fatalf("unexpected wake response: %d %s", wakeResponse.Code, wakeResponse.Body.String())
	}
	waitForProxyState(t, manager, supervisor.Running)
	request := httptest.NewRequest(http.MethodGet, "http://demo.test/hello?x=1", nil)
	request.Host = "demo.test"
	request.Header.Set("CF-Ray", "ray-123")
	request.Header.Set("CF-IPCountry", "hr")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body, _ := io.ReadAll(response.Result().Body)
	if response.Code != http.StatusOK || string(body) != "hello from demo ray-123" {
		t.Fatalf("unexpected proxy response: %d %s", response.Code, body)
	}
	if _, ok := response.Header()["X-Powered-By"]; ok || response.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers were not applied: %v", response.Header())
	}
	chunked := httptest.NewRequest(http.MethodPost, "http://demo.test/upload", struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 4096))})
	chunked.Host = "demo.test"
	chunkedResponse := httptest.NewRecorder()
	handler.ServeHTTP(chunkedResponse, chunked)
	if chunkedResponse.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked body over max_body: %d %s", chunkedResponse.Code, chunkedResponse.Body.String())
	}
	forwarded := httptest.NewRequest(http.MethodGet, "http://demo.test/headers", nil)
	forwarded.Host = "demo.test"
	forwarded.Header.Set("CF-Connecting-IP", "203.0.113.9")
	forwarded.Header.Set("CF-IPCountry", "not-a-country")
	forwardedResponse := httptest.NewRecorder()
	handler.ServeHTTP(forwardedResponse, forwarded)
	forwardedBody, _ := io.ReadAll(forwardedResponse.Result().Body)
	if forwardedResponse.Code != http.StatusOK || string(forwardedBody) != "proto=http;host=demo.test;real=203.0.113.9" {
		t.Fatalf("forwarded headers = %d %q", forwardedResponse.Code, forwardedBody)
	}
	time.Sleep(30 * time.Millisecond)
	rates, err := requestLogs.Rates("demo")
	if err != nil {
		t.Fatal(err)
	}
	if rates.LastMinute != 4 {
		t.Fatalf("requests were not logged: %+v", rates)
	}
	db, err := sql.Open("sqlite", filepath.Join(cfg.LogDir, "demo", "dboss.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var requestID, visitorCountry string
	if err := db.QueryRow(`SELECT request_id, country FROM requests WHERE path = '/hello?x=1'`).Scan(&requestID, &visitorCountry); err != nil || requestID != "ray-123" || visitorCountry != "HR" {
		t.Fatalf("request id row = %q %q, %v", requestID, visitorCountry, err)
	}
	if err := db.QueryRow(`SELECT country FROM requests WHERE path = '/headers'`).Scan(&visitorCountry); err != nil || visitorCountry != "" {
		t.Fatalf("junk country row = %q, %v", visitorCountry, err)
	}
	assertUpgradePassthrough(t, handler)
	assertAppErrorPage(t, handler)
	assertDbossPages(t, handler)
	if err := manager.Stop("demo"); err != nil {
		t.Fatal(err)
	}
}

// TestButtonAppWakesOnPost pins the autostart: button contract: a GET answers the start page
// without touching the app, and only the POST the button makes brings it up.
func TestButtonAppWakesOnPost(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "demo")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	appConfig := fmt.Sprintf("procfile:\n  web:\n    command: %s -test.run=TestProxyHelperProcess\n    hosts: [demo.test]\nautostart: button\n", os.Args[0])
	writeProxyFixture(t, filepath.Join(appDir, config.FileName), appConfig)
	writeProxyFixture(t, filepath.Join(appDir, ".env"), "BOSS_PROXY_HELPER=1\n")
	cfg := config.Default()
	cfg.Apps = filepath.Join(root, "apps")
	cfg.StateDir = filepath.Join(root, "state")
	cfg.LogDir = filepath.Join(root, "log")
	cfg.Socket = filepath.Join(root, "dboss.sock")
	cfg.Ports = [2]int{32300, 32320}
	cfg.Defaults.HealthTimeout = config.Duration(2 * time.Second)
	manager, invalid, err := supervisor.New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.Boot()
	defer manager.Close()
	if len(invalid) != 0 {
		t.Fatalf("invalid apps: %v", invalid)
	}
	requestLogs := logstore.New(cfg.LogDir, 10*time.Millisecond, nil, "", time.Hour, 0)
	defer requestLogs.Close()
	handler, err := New(cfg, authcog.NewWithKey([]byte("01234567890123456789012345678901")), manager, requestLogs, nil)
	if err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	get.Host = "demo.test"
	get.Header.Set("Accept", "text/html")
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusServiceUnavailable || !strings.Contains(getResponse.Body.String(), "Start demo") {
		t.Fatalf("stopped button page = %d %s", getResponse.Code, getResponse.Body.String())
	}
	if snapshot, _ := manager.Snapshot("demo"); snapshot.State != supervisor.Stopped {
		t.Fatalf("GET woke the button app: %+v", snapshot)
	}
	post := httptest.NewRequest(http.MethodPost, "http://demo.test/", nil)
	post.Host = "demo.test"
	post.Header.Set("Accept", "text/html")
	postResponse := httptest.NewRecorder()
	handler.ServeHTTP(postResponse, post)
	if postResponse.Code != http.StatusServiceUnavailable || !strings.Contains(postResponse.Body.String(), "demo is starting") {
		t.Fatalf("post wake response = %d %s", postResponse.Code, postResponse.Body.String())
	}
	waitForProxyState(t, manager, supervisor.Running)
}

// assertUpgradePassthrough drives a raw websocket-style upgrade through a real listener so the
// hijack path of the response recorder and the reverse proxy is exercised end to end.
func assertUpgradePassthrough(t *testing.T, handler http.Handler) {
	t.Helper()
	edge := httptest.NewServer(handler)
	defer edge.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(edge.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(conn, "GET /ws HTTP/1.1\r\nHost: demo.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(status, "HTTP/1.1 101") {
		t.Fatalf("upgrade status = %q, %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	fmt.Fprint(conn, "ping\n")
	echo, err := reader.ReadString('\n')
	if err != nil || echo != "echo ping\n" {
		t.Fatalf("echo = %q, %v", echo, err)
	}
}

// assertDbossPages pins the pages dboss answers without an app: the logo on any host, and the
// built-in 404 for a host no app owns.
func assertDbossPages(t *testing.T, handler *Handler) {
	t.Helper()
	for _, host := range []string{"demo.test", "nobody.test"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://"+host+"/.well-known/dboss/logo.svg", nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(response.Body.String(), "<svg") {
			t.Fatalf("logo on %s = %d %v", host, response.Code, response.Header())
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://nobody.test/", nil))
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "Nothing here") {
		t.Fatalf("unknown host = %d %s", response.Code, response.Body.String())
	}
}

// assertAppErrorPage pins the app's error page against a live app: a 5xx answer to an HTML GET gets
// public/error_pages/error.html with the app's status, and nothing else the app says is rewritten.
func assertAppErrorPage(t *testing.T, handler *Handler) {
	t.Helper()
	call := func(method, target, accept string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "http://demo.test"+target, nil)
		request.Host = "demo.test"
		if accept != "" {
			request.Header.Set("Accept", accept)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	page := call(http.MethodGet, "/boom", "text/html,application/xhtml+xml")
	if page.Code != http.StatusInternalServerError || page.Body.String() != "<h1>custom error demo 500</h1>" {
		t.Fatalf("app 500 on an html GET = %d %q", page.Code, page.Body.String())
	}
	if got := page.Header(); !strings.Contains(got.Get("Content-Type"), "text/html") || got.Get("Cache-Control") != "no-store" || got.Get("Content-Encoding") != "" || got.Get("Etag") != "" || got.Get("Content-Length") != strconv.Itoa(page.Body.Len()) {
		t.Fatalf("app 500 headers = %v", got)
	}
	if api := call(http.MethodGet, "/boom", "application/json"); api.Code != http.StatusInternalServerError || !strings.Contains(api.Body.String(), "stack trace") {
		t.Fatalf("app 500 on a json GET must pass through: %d %q", api.Code, api.Body.String())
	}
	if post := call(http.MethodPost, "/boom", "text/html"); post.Code != http.StatusInternalServerError || !strings.Contains(post.Body.String(), "stack trace") {
		t.Fatalf("app 500 on a POST must pass through: %d %q", post.Code, post.Body.String())
	}
	if missing := call(http.MethodGet, "/gone", "text/html"); missing.Code != http.StatusNotFound || missing.Body.String() != "app 404" {
		t.Fatalf("app 404 must pass through: %d %q", missing.Code, missing.Body.String())
	}
}

func TestProxyHelperProcess(t *testing.T) {
	if os.Getenv("BOSS_PROXY_HELPER") != "1" {
		return
	}
	http.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "identity")
		w.Header().Set("Etag", `"boom"`)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "stack trace: boom")
	})
	http.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "app 404")
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Powered-By", "helper")
		_, _ = io.WriteString(w, "hello from demo "+r.Header.Get("X-Request-ID"))
	})
	http.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, "uploaded")
	})
	http.HandleFunc("/headers", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "proto=%s;host=%s;real=%s", r.Header.Get("X-Forwarded-Proto"), r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Real-IP"))
	})
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "upgrade required", http.StatusBadRequest)
			return
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = buffered.Flush()
		line, err := buffered.ReadString('\n')
		if err != nil {
			return
		}
		_, _ = buffered.WriteString("echo " + line)
		_ = buffered.Flush()
	})
	if err := http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), nil); err != nil {
		os.Exit(2)
	}
}

func waitForProxyState(t *testing.T, manager *supervisor.Manager, state supervisor.State) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot, err := manager.Snapshot("demo")
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.State == state {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("app did not reach %s: %+v", state, snapshot)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func writeProxyFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
		t.Fatal(err)
	}
}

type fakeAuthorizer struct {
	allow  bool
	called bool
}

func (f *fakeAuthorizer) AuthorizesPublish(*http.Request, supervisor.Snapshot) bool {
	f.called = true
	return f.allow
}

// A pubsub publisher presents its own secret, so basic_auth must not reject it before the
// publisher filter runs.
func TestAuthorizeHonorsPublishAuthorizer(t *testing.T) {
	app := supervisor.Snapshot{Name: "web", Web: config.Web{BasicAuth: map[string]string{"alice": "$2a$10$abcdefghijklmnopqrstuv"}}}

	authorizer := &fakeAuthorizer{allow: true}
	handler := &Handler{pubsub: authorizer}
	next := false
	recorder := httptest.NewRecorder()
	handler.authorize(recorder, httptest.NewRequest(http.MethodPost, "/socketio/chat", nil), app, func() { next = true })
	if !next || !authorizer.called {
		t.Fatalf("publish authorizer was not consulted: next=%v called=%v", next, authorizer.called)
	}

	denied := &fakeAuthorizer{allow: false}
	handler = &Handler{pubsub: denied}
	recorder = httptest.NewRecorder()
	next = false
	handler.authorize(recorder, httptest.NewRequest(http.MethodPost, "/socketio/chat", nil), app, func() { next = true })
	if next {
		t.Fatal("an unauthorized publish must not reach the next filter")
	}
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}

func TestSuppressRecordSkipsRequestLog(t *testing.T) {
	recorder := &responseRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	recorder.SuppressRecord()
	if !recorder.suppressed {
		t.Fatal("SuppressRecord did not mark the recorder")
	}
}
