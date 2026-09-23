// Package notify forwards notable runtime events to an operator webhook. Sends are debounced,
// queued and best-effort, so a slow or dead endpoint can never stall the supervisor.
package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

// The events an operator can subscribe to with notify.events.
const (
	Crash         = "crash"
	RestartLoop   = "restart-loop"
	HealthTimeout = "health-timeout"
	WakeFailed    = "wake-failed"
	HookFailed    = "hook-failed"
	CronFailed    = "cron-failed"
	Deploy        = "deploy"
	ConfigChanged = "config-changed"
	BackupFailed  = "backup-failed"
	ErrorRate     = "error-rate"
	Slow          = "slow"
	OOM           = "oom"
	DiskLow       = "disk-low"
)

// Events lists every event; the default notify.events subscribes to all of them.
var Events = []string{Crash, RestartLoop, HealthTimeout, WakeFailed, HookFailed, CronFailed, Deploy, ConfigChanged, BackupFailed, ErrorRate, Slow, OOM, DiskLow}

// Sink is what the supervisor calls. A disabled notifier drops the event.
type Sink interface {
	Send(Event)
}

// FormatFor picks the payload shape from the webhook address: Slack, Discord and ntfy get their
// own, anything else the raw event JSON.
func FormatFor(webhook string) string {
	parsed, err := url.Parse(webhook)
	if err != nil {
		return "generic"
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case host == "hooks.slack.com":
		return "slack"
	case (host == "discord.com" || host == "discordapp.com" || strings.HasSuffix(host, ".discord.com")) && strings.HasPrefix(parsed.Path, "/api/webhooks/"):
		return "discord"
	case host == "ntfy.sh" || strings.HasPrefix(host, "ntfy."):
		return "ntfy"
	}
	return "generic"
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

	mu     sync.Mutex
	last   map[string]time.Time
	closed bool

	sent    atomic.Int64
	failed  atomic.Int64
	dropped atomic.Int64

	queue   chan Event
	done    chan struct{}
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
	n.done = make(chan struct{})
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
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	if n.cfg.MinInterval > 0 {
		if last, ok := n.last[key]; ok && event.Time.Sub(last) < n.cfg.MinInterval {
			return
		}
	}
	select {
	case n.queue <- event:
		// Only a queued event starts the quiet period; a dropped one must not suppress the next.
		n.last[key] = event.Time
	default:
		n.dropped.Add(1)
	}
}

// Close stops accepting events and waits for the worker to drain the queue. Safe to call more
// than once.
func (n *Notifier) Close() {
	if n.queue == nil {
		return
	}
	n.closeMu.Do(func() {
		n.mu.Lock()
		n.closed = true
		close(n.queue)
		n.mu.Unlock()
		<-n.done
	})
}

// Stats reports the counters since the notifier started.
func (n *Notifier) Stats() Stats {
	return Stats{Sent: n.sent.Load(), Failed: n.failed.Load(), Dropped: n.dropped.Load()}
}

func (n *Notifier) run() {
	defer close(n.done)
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
	// One retry covers a transient DNS or connection error without holding the queue long. A
	// client error is final: retrying a bad URL or payload only delays the next event.
	for attempt := 0; attempt < 2; attempt++ {
		err = n.request(body)
		if err == nil {
			return nil
		}
		var status httpStatusError
		if errors.As(err, &status) && status.code >= 400 && status.code < 500 && status.code != http.StatusTooManyRequests {
			return err
		}
		if attempt == 0 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return err
}

type httpStatusError struct{ code int }

func (e httpStatusError) Error() string { return fmt.Sprintf("webhook returned %d", e.code) }

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
		return httpStatusError{code: response.StatusCode}
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
		return json.Marshal(map[string]any{"title": "dboss " + event.Type, "message": message, "tags": []string{event.Type}})
	default:
		return json.Marshal(event)
	}
}
