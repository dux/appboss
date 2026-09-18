// Package notify forwards notable runtime events to an operator webhook. Sends are debounced,
// queued and best-effort, so a slow or dead endpoint can never stall the supervisor.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one thing worth telling an operator about.
type Event struct {
	Type  string    `json:"event"`
	App   string    `json:"app"`
	Error string    `json:"error,omitempty"`
	Time  time.Time `json:"time"`
}

// Sink is what the supervisor calls. A nil or disabled notifier drops the event.
type Sink interface {
	Send(Event)
}

// Config is the notify: block of the host config.
type Config struct {
	URL         string
	Format      string
	Events      []string
	MinInterval time.Duration
	Headers     map[string]string
}

// Stats counts what the notifier did, for the metrics endpoint.
type Stats struct {
	Sent    int64 `json:"sent"`
	Failed  int64 `json:"failed"`
	Dropped int64 `json:"dropped"`
}

const queueSize = 128

// Notifier posts events to one webhook. A zero URL disables it: New returns a sink whose Send is
// a no-op.
type Notifier struct {
	cfg     Config
	client  *http.Client
	enabled map[string]bool

	mu   sync.Mutex
	last map[string]time.Time

	sent    atomic.Int64
	failed  atomic.Int64
	dropped atomic.Int64

	queue   chan Event
	closeMu sync.Once
}

func New(cfg Config) *Notifier {
	n := &Notifier{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}, enabled: map[string]bool{}, last: map[string]time.Time{}}
	if cfg.URL == "" {
		return n
	}
	for _, event := range cfg.Events {
		n.enabled[event] = true
	}
	n.queue = make(chan Event, queueSize)
	go n.run()
	return n
}

// Send queues one event. It never blocks: a debounced or full queue drops the event.
func (n *Notifier) Send(event Event) {
	if n.cfg.URL == "" || !n.enabled[event.Type] {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	key := event.App + "/" + event.Type
	n.mu.Lock()
	if n.cfg.MinInterval > 0 {
		if last, ok := n.last[key]; ok && event.Time.Sub(last) < n.cfg.MinInterval {
			n.mu.Unlock()
			return
		}
	}
	n.last[key] = event.Time
	n.mu.Unlock()
	select {
	case n.queue <- event:
	default:
		n.dropped.Add(1)
	}
}

// Close drains the queue and stops the worker. It is safe to call more than once.
func (n *Notifier) Close() {
	if n.queue == nil {
		return
	}
	n.closeMu.Do(func() { close(n.queue) })
}

// Stats reports the counters since the notifier started.
func (n *Notifier) Stats() Stats {
	return Stats{Sent: n.sent.Load(), Failed: n.failed.Load(), Dropped: n.dropped.Load()}
}

func (n *Notifier) run() {
	for event := range n.queue {
		if err := n.post(event); err != nil {
			n.failed.Add(1)
			continue
		}
		n.sent.Add(1)
	}
}

func (n *Notifier) post(event Event) error {
	body, err := n.body(event)
	if err != nil {
		return err
	}
	// One retry covers a transient DNS or connection error without holding the queue long.
	for attempt := 0; attempt < 2; attempt++ {
		err = n.request(body)
		if err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

func (n *Notifier) request(body []byte) error {
	request, err := http.NewRequest(http.MethodPost, n.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range n.cfg.Headers {
		request.Header.Set(name, value)
	}
	response, err := n.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", response.StatusCode)
	}
	return nil
}

// body renders the event for the configured format. Slack and Discord want a text field, ntfy
// wants title and message, everything else gets the raw event JSON.
func (n *Notifier) body(event Event) ([]byte, error) {
	message := event.Type + ": " + event.App
	if event.Error != "" {
		message += ": " + event.Error
	}
	switch n.cfg.Format {
	case "slack":
		return json.Marshal(map[string]string{"text": message})
	case "discord":
		return json.Marshal(map[string]string{"content": message})
	case "ntfy":
		return json.Marshal(map[string]any{"title": "appboss " + event.Type, "message": message, "tags": []string{event.Type}})
	default:
		return json.Marshal(event)
	}
}
