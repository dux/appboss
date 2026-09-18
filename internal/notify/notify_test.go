package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// capture is a webhook test server that records every request body.
type capture struct {
	mu       sync.Mutex
	bodies   [][]byte
	headers  []http.Header
	received chan struct{}
}

func newCapture() *capture {
	return &capture{received: make(chan struct{}, 16)}
}

func (c *capture) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.headers = append(c.headers, r.Header.Clone())
		c.mu.Unlock()
		c.received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
}

func (c *capture) wait(t *testing.T) []byte {
	t.Helper()
	select {
	case <-c.received:
	case <-time.After(3 * time.Second):
		t.Fatal("webhook was not called")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[len(c.bodies)-1]
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func testConfig(url string) Config {
	return Config{URL: url, Format: "generic", Events: []string{"crash", "restart-loop", "health-timeout", "wake-failed", "hook-failed"}, MinInterval: time.Minute}
}

func TestSendPostsGenericEvent(t *testing.T) {
	server := newCapture()
	httpServer := server.server()
	defer httpServer.Close()

	notifier := New(testConfig(httpServer.URL))
	defer notifier.Close()
	notifier.Send(Event{Type: "crash", App: "web", Error: "boom", Time: time.Now()})

	var event map[string]any
	if err := json.Unmarshal(server.wait(t), &event); err != nil {
		t.Fatal(err)
	}
	if event["event"] != "crash" || event["app"] != "web" || event["error"] != "boom" {
		t.Fatalf("payload = %v", event)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && notifier.Stats().Sent == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if stats := notifier.Stats(); stats.Sent != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestSendFormats(t *testing.T) {
	for format, want := range map[string]string{
		"slack":   `"text":"crash: web: boom"`,
		"discord": `"content":"crash: web: boom"`,
		"ntfy":    `"message":"crash: web: boom"`,
	} {
		server := newCapture()
		httpServer := server.server()
		cfg := testConfig(httpServer.URL)
		cfg.Format = format
		notifier := New(cfg)
		notifier.Send(Event{Type: "crash", App: "web", Error: "boom"})
		body := string(server.wait(t))
		if !strings.Contains(body, want) {
			t.Errorf("%s payload = %s, want %s", format, body, want)
		}
		notifier.Close()
		httpServer.Close()
	}
}

func TestDebounceDropsRepeatedEvents(t *testing.T) {
	server := newCapture()
	httpServer := server.server()
	defer httpServer.Close()

	notifier := New(testConfig(httpServer.URL))
	defer notifier.Close()
	now := time.Now()
	notifier.Send(Event{Type: "crash", App: "web", Time: now})
	notifier.Send(Event{Type: "crash", App: "web", Time: now.Add(time.Second)})
	server.wait(t)
	time.Sleep(200 * time.Millisecond)
	if got := server.count(); got != 1 {
		t.Fatalf("webhook called %d times, want 1", got)
	}
}

func TestSendAfterCloseIsSafe(t *testing.T) {
	server := newCapture()
	httpServer := server.server()
	defer httpServer.Close()
	notifier := New(testConfig(httpServer.URL))
	notifier.Close()
	notifier.Send(Event{Type: "crash", App: "web"})
	notifier.Close()
}

func TestClientErrorIsNotRetried(t *testing.T) {
	var requests atomic.Int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer httpServer.Close()
	notifier := New(testConfig(httpServer.URL))
	defer notifier.Close()
	notifier.Send(Event{Type: "crash", App: "web"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && notifier.Stats().Failed == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Fatalf("a 4xx must not be retried: %d requests", got)
	}
}

func TestSendNoopsWithoutURL(t *testing.T) {
	notifier := New(Config{Format: "generic", Events: []string{"crash"}})
	notifier.Send(Event{Type: "crash", App: "web"})
	notifier.Close()
	if stats := notifier.Stats(); stats != (Stats{}) {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestSendIgnoresUnselectedEvents(t *testing.T) {
	server := newCapture()
	httpServer := server.server()
	defer httpServer.Close()

	cfg := testConfig(httpServer.URL)
	cfg.Events = []string{"crash"}
	notifier := New(cfg)
	defer notifier.Close()
	notifier.Send(Event{Type: "health-timeout", App: "web"})
	time.Sleep(200 * time.Millisecond)
	if got := server.count(); got != 0 {
		t.Fatalf("webhook called %d times, want 0", got)
	}
}
