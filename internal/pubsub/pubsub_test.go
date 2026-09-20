package pubsub

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"dboss/internal/config"
	"dboss/internal/super"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	service, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func snapshotFor(name string, cfg config.Pubsub) super.Snapshot {
	return super.Snapshot{
		Name:         name,
		Hosts:        []string{"app.test"},
		WebProcesses: []super.WebProcessSnapshot{{Name: "web", Hosts: []string{"app.test"}, Pubsub: cfg}},
	}
}

func filterHandler(service *Service, snapshot super.Snapshot) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "app.test"
		service.Filter(w, r, snapshot, func() { w.WriteHeader(http.StatusTeapot) })
	})
}

func wsURL(server *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(server.URL, "http") + path
}

func publishMessage(t *testing.T, server *httptest.Server, secret, path, body string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+secret)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("publish status = %d, want 202", response.StatusCode)
	}
}

func TestWebSocketPublishAndSubscribe(t *testing.T) {
	service := newTestService(t)
	cfg := config.Pubsub{Path: "/socketio", Secret: "s3cret", Replay: 10}
	snapshot := snapshotFor("web", cfg)
	server := httptest.NewServer(filterHandler(service, snapshot))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(server, "/socketio/chat"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	publishMessage(t, server, "s3cret", "/socketio/chat", `{"event":"greeting","data":{"n":1}}`)

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var message Message
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	if message.Event != "greeting" || !strings.Contains(string(message.Data), `"n":1`) {
		t.Fatalf("unexpected message: %+v", message)
	}
}

func TestSSEPublishAndSubscribe(t *testing.T) {
	service := newTestService(t)
	cfg := config.Pubsub{Path: "/socketio", Secret: "s3cret", Replay: 10}
	snapshot := snapshotFor("web", cfg)
	server := httptest.NewServer(filterHandler(service, snapshot))
	defer server.Close()

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/socketio/chat", nil)
	request.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	lines := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(response.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "data: ") {
				lines <- strings.TrimSpace(strings.TrimPrefix(line, "data: "))
				return
			}
		}
	}()

	publishMessage(t, server, "s3cret", "/socketio/chat", `{"event":"hello","data":"sse"}`)
	select {
	case data := <-lines:
		if !strings.Contains(data, `"event":"hello"`) {
			t.Fatalf("unexpected SSE frame: %s", data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an SSE frame")
	}
}

func TestReplayToLateSubscriber(t *testing.T) {
	service := newTestService(t)
	cfg := config.Pubsub{Path: "/socketio", Secret: "s3cret", Replay: 2}
	snapshot := snapshotFor("web", cfg)
	server := httptest.NewServer(filterHandler(service, snapshot))
	defer server.Close()

	publishMessage(t, server, "s3cret", "/socketio/chat", `{"event":"m1"}`)
	publishMessage(t, server, "s3cret", "/socketio/chat", `{"event":"m2"}`)
	publishMessage(t, server, "s3cret", "/socketio/chat", `{"event":"m3"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(server, "/socketio/chat"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	var events []string
	for i := 0; i < 2; i++ {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var message Message
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		if !message.Replay {
			t.Fatalf("message %q should be marked replay", message.Event)
		}
		events = append(events, message.Event)
	}
	if strings.Join(events, ",") != "m2,m3" {
		t.Fatalf("replay = %v, want [m2 m3]", events)
	}
}

func TestClientEvents(t *testing.T) {
	service := newTestService(t)
	cfg := config.Pubsub{Path: "/socketio", Secret: "s3cret", Replay: 0, ClientEvents: true}
	snapshot := snapshotFor("web", cfg)
	server := httptest.NewServer(filterHandler(service, snapshot))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(server, "/socketio/chat"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"event":"ping","data":{"who":"a"}}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var message Message
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	if message.Event != "ping" || !strings.Contains(string(message.Data), `"who":"a"`) {
		t.Fatalf("client event not echoed: %+v", message)
	}
}

func TestPublishRequiresSecret(t *testing.T) {
	service := newTestService(t)
	cfg := config.Pubsub{Path: "/socketio", Secret: "s3cret"}
	snapshot := snapshotFor("web", cfg)
	server := httptest.NewServer(filterHandler(service, snapshot))
	defer server.Close()

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/socketio/chat", strings.NewReader(`{}`))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
}

func TestGeneratedSecret(t *testing.T) {
	service := newTestService(t)
	cfg := config.Pubsub{Path: "/socketio"}
	snapshot := snapshotFor("web", cfg)
	secret, err := service.Secret("web", "web", cfg)
	if err != nil || len(secret) != 64 {
		t.Fatalf("generated secret = %q, %v", secret, err)
	}
	server := httptest.NewServer(filterHandler(service, snapshot))
	defer server.Close()
	publishMessage(t, server, secret, "/socketio/chat", `{"event":"x"}`)

	if _, err := service.Rotate("web", "web", config.Pubsub{Path: "/socketio", Secret: "from-config"}); err == nil {
		t.Fatal("rotating a config secret should fail")
	}
}

func TestReservedRoutes(t *testing.T) {
	service := newTestService(t)
	cfg := config.Pubsub{Path: "/socketio", Secret: "s3cret", Test: true}
	server := httptest.NewServer(filterHandler(service, snapshotFor("web", cfg)))
	defer server.Close()

	for _, test := range []struct {
		path string
		want int
	}{
		{"/socketio/client.js", http.StatusOK},
		{"/socketio/_test", http.StatusOK},
		{"/socketio/a/b", http.StatusTeapot},
		{"/socketio/chat", http.StatusUpgradeRequired},
	} {
		response, err := http.Get(server.URL + test.path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.want {
			t.Errorf("GET %s = %d, want %d", test.path, response.StatusCode, test.want)
		}
	}

	noTest := httptest.NewServer(filterHandler(service, snapshotFor("web", config.Pubsub{Path: "/socketio"})))
	defer noTest.Close()
	response, err := http.Get(noTest.URL + "/socketio/_test")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTeapot {
		t.Errorf("self-test with test: false = %d, want the next handler (418)", response.StatusCode)
	}
}

func TestSelfTestPublish(t *testing.T) {
	service := newTestService(t)
	cfg := config.Pubsub{Path: "/socketio", Test: true}
	snapshot := snapshotFor("web", cfg)
	server := httptest.NewServer(filterHandler(service, snapshot))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(server, "/socketio/_selftest"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	response, err := http.Post(server.URL+"/socketio/_test/publish", "application/json", strings.NewReader(`{"nonce":"abc"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("self-test publish = %d, want 202", response.StatusCode)
	}

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var message Message
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	if message.Event != "selftest" || !strings.Contains(string(message.Data), `"nonce":"abc"`) {
		t.Fatalf("self-test message = %+v", message)
	}
}

func TestMaxClients(t *testing.T) {
	service := newTestService(t)
	cfg := config.Pubsub{Path: "/socketio", Secret: "s3cret", MaxClients: 1}
	snapshot := snapshotFor("web", cfg)
	server := httptest.NewServer(filterHandler(service, snapshot))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, _, err := websocket.Dial(ctx, wsURL(server, "/socketio/chat"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.CloseNow()

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/socketio/chat", nil)
	request.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second subscriber = %d, want 503", response.StatusCode)
	}
}

func TestAuthorizesPublish(t *testing.T) {
	service := newTestService(t)
	cfg := config.Pubsub{Path: "/socketio", Secret: "s3cret"}
	snapshot := snapshotFor("web", cfg)

	request, _ := http.NewRequest(http.MethodPost, "http://app.test/socketio/chat", strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer s3cret")
	if !service.AuthorizesPublish(request, snapshot) {
		t.Fatal("valid publish should be authorized")
	}

	get, _ := http.NewRequest(http.MethodGet, "http://app.test/socketio/chat", nil)
	if service.AuthorizesPublish(get, snapshot) {
		t.Fatal("GET is not a publish")
	}

	wrong, _ := http.NewRequest(http.MethodPost, "http://app.test/socketio/chat", strings.NewReader("{}"))
	wrong.Header.Set("Authorization", "Bearer nope")
	if service.AuthorizesPublish(wrong, snapshot) {
		t.Fatal("wrong secret must not be authorized")
	}
}

func TestAuthorizesPublishPerWebProcess(t *testing.T) {
	service := newTestService(t)
	snapshot := super.Snapshot{
		Name:  "app",
		Hosts: []string{"a.test", "b.test"},
		WebProcesses: []super.WebProcessSnapshot{
			{Name: "a", Hosts: []string{"a.test"}, Pubsub: config.Pubsub{Path: "/socketio", Secret: "sa"}},
			{Name: "b", Hosts: []string{"b.test"}, Pubsub: config.Pubsub{Path: "/socketio", Secret: "sb"}},
		},
	}
	post := func(host, secret string) bool {
		request, _ := http.NewRequest(http.MethodPost, "http://"+host+"/socketio/chat", strings.NewReader("{}"))
		request.Host = host
		request.Header.Set("Authorization", "Bearer "+secret)
		return service.AuthorizesPublish(request, snapshot)
	}
	if !post("a.test", "sa") {
		t.Fatal("a's secret should authorize a")
	}
	if post("a.test", "sb") {
		t.Fatal("b's secret must not authorize a")
	}
	if !post("b.test", "sb") {
		t.Fatal("b's secret should authorize b")
	}
}

func TestHubsHaveOwnGeneratedSecrets(t *testing.T) {
	service := newTestService(t)
	a, err := service.Secret("app", "a", config.Pubsub{Path: "/socketio"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := service.Secret("app", "b", config.Pubsub{Path: "/socketio"})
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || b == "" || a == b {
		t.Fatalf("generated secrets must be per web process: %q %q", a, b)
	}
}

func TestDisabledPathFallsThrough(t *testing.T) {
	service := newTestService(t)
	snapshot := snapshotFor("web", config.Pubsub{})
	server := httptest.NewServer(filterHandler(service, snapshot))
	defer server.Close()

	response, err := http.Get(server.URL + "/socketio/chat")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTeapot {
		t.Fatalf("disabled pubsub = %d, want the next handler (418)", response.StatusCode)
	}
}
