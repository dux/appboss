package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"deploy-boss/internal/config"
	"deploy-boss/internal/ports"
	"deploy-boss/internal/reqlog"
	"deploy-boss/internal/super"
)

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "http://example.test", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")
	if got := clientIP(r, []string{"CF-Connecting-IP"}); got != "203.0.113.9" {
		t.Fatalf("got %q", got)
	}
}

func TestWakeProxyAndRequestLog(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "demo")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	appConfig := fmt.Sprintf("procfile:\n  web: %s -test.run=TestProxyHelperProcess\nhosts: [demo.test]\n", os.Args[0])
	writeProxyFixture(t, filepath.Join(appDir, config.FileName), appConfig)
	writeProxyFixture(t, filepath.Join(appDir, ".env"), "BOSS_PROXY_HELPER=1\n")
	cfg := config.Default()
	cfg.Apps = filepath.Join(root, "apps")
	cfg.StateDir = filepath.Join(root, "state")
	cfg.LogDir = filepath.Join(root, "log")
	cfg.Socket = filepath.Join(root, "dboss.sock")
	cfg.Ports.Range = [2]int{32200, 32220}
	cfg.Defaults.HealthInterval = config.Duration(10 * time.Millisecond)
	cfg.Defaults.HealthTimeout = config.Duration(2 * time.Second)
	cfg.Defaults.LogFlush = config.Duration(10 * time.Millisecond)
	manager, invalid, err := super.New(cfg, ports.New(cfg.Ports.Range), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(invalid) != 0 {
		t.Fatalf("invalid apps: %v", invalid)
	}
	requestLogs := reqlog.New(cfg.LogDir, 10*time.Millisecond)
	defer requestLogs.Close()
	handler, err := New(cfg, manager, requestLogs)
	if err != nil {
		t.Fatal(err)
	}
	wakeRequest := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	wakeRequest.Host = "demo.test"
	wakeRequest.Header.Set("Accept", "text/html")
	wakeResponse := httptest.NewRecorder()
	handler.ServeHTTP(wakeResponse, wakeRequest)
	if wakeResponse.Code != http.StatusServiceUnavailable || !strings.Contains(wakeResponse.Body.String(), "Starting demo") {
		t.Fatalf("unexpected wake response: %d %s", wakeResponse.Code, wakeResponse.Body.String())
	}
	waitForProxyState(t, manager, super.Running)
	request := httptest.NewRequest(http.MethodGet, "http://demo.test/hello?x=1", nil)
	request.Host = "demo.test"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body, _ := io.ReadAll(response.Result().Body)
	if response.Code != http.StatusOK || string(body) != "hello from demo" {
		t.Fatalf("unexpected proxy response: %d %s", response.Code, body)
	}
	time.Sleep(30 * time.Millisecond)
	rates, err := requestLogs.Rates("demo")
	if err != nil {
		t.Fatal(err)
	}
	if rates.LastMinute != 1 {
		t.Fatalf("request was not logged: %+v", rates)
	}
	assertUpgradePassthrough(t, handler)
	if err := manager.Stop("demo"); err != nil {
		t.Fatal(err)
	}
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

func TestProxyHelperProcess(t *testing.T) {
	if os.Getenv("BOSS_PROXY_HELPER") != "1" {
		return
	}
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello from demo") })
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

func waitForProxyState(t *testing.T, manager *super.Manager, state super.State) {
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
	if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
		t.Fatal(err)
	}
}
