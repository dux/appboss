// Package pubsub serves realtime channels for an app on the app's own hosts. A publisher posts a
// message over HTTP with the app's publish secret and every subscriber of that channel receives
// it, over a WebSocket or SSE. It is independent of the app process, so channels work while the
// app is stopped and realtime traffic never wakes it.
package pubsub

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"dboss/internal/config"
	"dboss/internal/super"
)

// outBuffer is how many messages may queue for one slow subscriber before it is dropped.
const outBuffer = 64

// errTooManyClients is returned when an app has reached its max_clients.
var errTooManyClients = errors.New("subscriber limit reached")

// Message is one frame on a channel. Data is the publisher's payload, kept as raw JSON so the
// hub never re-encodes it.
type Message struct {
	Event  string          `json:"event"`
	Data   json.RawMessage `json:"data,omitempty"`
	TS     time.Time       `json:"ts"`
	Replay bool            `json:"replay,omitempty"`
}

// Stats is one app's live counters for /metrics.
type Stats struct {
	Clients  int
	Channels int
	Messages uint64
}

// Channel and App are the console and CLI view of the hub, without any secret.
type Channel struct {
	Name        string `json:"name"`
	Subscribers int    `json:"subscribers"`
	Messages    uint64 `json:"messages"`
}

type App struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Clients  int       `json:"clients"`
	Channels []Channel `json:"channels"`
}

// Service owns every app's channels. It is a daemon module: Start runs the janitor that drops
// idle empty channels, Close disconnects every subscriber.
type Service struct {
	mu      sync.Mutex
	apps    map[string]*appHub
	secrets *secretStore
	client  []byte
}

func New(stateDir string) (*Service, error) {
	secrets, err := openSecrets(stateDir)
	if err != nil {
		return nil, err
	}
	return &Service{apps: map[string]*appHub{}, secrets: secrets, client: clientJS}, nil
}

func (s *Service) Name() string { return "pubsub" }

func (s *Service) Start(ctx context.Context) error {
	go s.janitor(ctx)
	return nil
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, hub := range s.apps {
		for _, ch := range hub.channels {
			for sub := range ch.subs {
				sub.stop()
			}
		}
	}
	s.apps = map[string]*appHub{}
	return nil
}

// Secret returns the effective publish secret: the one in the config, or a generated per-app one.
func (s *Service) Secret(app string, cfg config.Pubsub) (string, error) {
	if cfg.Secret != "" {
		return cfg.Secret, nil
	}
	return s.secrets.ensure(app)
}

// Rotate mints a new generated secret. A secret that comes from the config is rejected.
func (s *Service) Rotate(app string, cfg config.Pubsub) (string, error) {
	if cfg.Secret != "" {
		return "", errors.New("secret comes from the config; change it there")
	}
	secret, err := generateSecret()
	if err != nil {
		return "", err
	}
	if err := s.secrets.set(app, secret); err != nil {
		return "", err
	}
	return secret, nil
}

// Publish sends a message to a channel on behalf of the CLI or console.
func (s *Service) Publish(app, channel string, msg Message, replay int) int {
	return s.publish(app, channel, msg, replay)
}

// Reconcile drops generated secrets for apps that no longer serve channels.
func (s *Service) Reconcile(snapshots []super.Snapshot) {
	live := make(map[string]bool, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.Web.Pubsub.Enabled() {
			live[snapshot.Name] = true
		}
	}
	_ = s.secrets.reconcile(live)
}

// Stats is per-app counters, keyed by app name.
func (s *Service) Stats() map[string]Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]Stats, len(s.apps))
	for name, hub := range s.apps {
		result[name] = Stats{Clients: hub.clients, Channels: len(hub.channels), Messages: hub.messages}
	}
	return result
}

// Snapshot lists every app that serves channels, with its channels and subscriber counts.
func (s *Service) Snapshot(snapshots []super.Snapshot) []App {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]App, 0, len(snapshots))
	for _, snapshot := range snapshots {
		cfg := snapshot.Web.Pubsub
		if !cfg.Enabled() {
			continue
		}
		entry := App{Name: snapshot.Name, Path: cfg.Path, Channels: []Channel{}}
		if hub := s.apps[snapshot.Name]; hub != nil {
			entry.Clients = hub.clients
			for name, ch := range hub.channels {
				entry.Channels = append(entry.Channels, Channel{Name: name, Subscribers: len(ch.subs), Messages: ch.messages})
			}
			sort.Slice(entry.Channels, func(i, j int) bool { return entry.Channels[i].Name < entry.Channels[j].Name })
		}
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

type appHub struct {
	channels map[string]*channel
	clients  int
	messages uint64
}

type channel struct {
	subs     map[*subscriber]struct{}
	ring     []Message
	messages uint64
	last     time.Time
}

type subscriber struct {
	out  chan Message
	quit chan struct{}
	once sync.Once
}

func newSubscriber() *subscriber {
	return &subscriber{out: make(chan Message, outBuffer), quit: make(chan struct{})}
}

func (s *subscriber) stop() { s.once.Do(func() { close(s.quit) }) }

func (s *Service) subscribe(app, name string, cfg config.Pubsub) (*subscriber, []Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hub := s.hubLocked(app)
	if cfg.MaxClients > 0 && hub.clients >= cfg.MaxClients {
		return nil, nil, errTooManyClients
	}
	ch := hub.channelLocked(name)
	sub := newSubscriber()
	ch.subs[sub] = struct{}{}
	ch.last = time.Now()
	hub.clients++
	if cfg.Replay <= 0 {
		return sub, nil, nil
	}
	return sub, ch.backlogLocked(), nil
}

func (s *Service) unsubscribe(app, name string, sub *subscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hub := s.apps[app]
	if hub == nil {
		sub.stop()
		return
	}
	ch := hub.channels[name]
	if ch == nil {
		sub.stop()
		return
	}
	if _, ok := ch.subs[sub]; ok {
		delete(ch.subs, sub)
		hub.clients--
	}
	sub.stop()
	if len(ch.subs) == 0 && len(ch.ring) == 0 {
		delete(hub.channels, name)
	}
}

// publish fans msg out to every subscriber of the channel and keeps it for replay. A subscriber
// whose buffer is full is dropped instead of blocking the publisher. It returns the delivery count.
func (s *Service) publish(app, name string, msg Message, replay int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	hub := s.hubLocked(app)
	ch := hub.channelLocked(name)
	msg.TS = time.Now().UTC()
	ch.addLocked(msg, replay)
	hub.messages++
	ch.messages++
	delivered := 0
	var dropped []*subscriber
	for sub := range ch.subs {
		select {
		case sub.out <- msg:
			delivered++
		default:
			dropped = append(dropped, sub)
		}
	}
	for _, sub := range dropped {
		delete(ch.subs, sub)
		hub.clients--
		sub.stop()
	}
	return delivered
}

func (s *Service) hubLocked(app string) *appHub {
	hub := s.apps[app]
	if hub == nil {
		hub = &appHub{channels: map[string]*channel{}}
		s.apps[app] = hub
	}
	return hub
}

func (h *appHub) channelLocked(name string) *channel {
	ch := h.channels[name]
	if ch == nil {
		ch = &channel{subs: map[*subscriber]struct{}{}}
		h.channels[name] = ch
	}
	return ch
}

func (c *channel) addLocked(msg Message, replay int) {
	c.last = msg.TS
	if replay <= 0 {
		return
	}
	if len(c.ring) >= replay {
		copy(c.ring, c.ring[1:replay])
		c.ring[replay-1] = msg
		c.ring = c.ring[:replay]
		return
	}
	c.ring = append(c.ring, msg)
}

func (c *channel) backlogLocked() []Message {
	if len(c.ring) == 0 {
		return nil
	}
	out := make([]Message, len(c.ring))
	for i, msg := range c.ring {
		msg.Replay = true
		out[i] = msg
	}
	return out
}

// janitor drops channels that have had no subscribers and no new messages for an hour, so a busy
// publisher cannot grow one app's channel map without bound.
func (s *Service) janitor(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.prune(now)
		}
	}
}

func (s *Service) prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, hub := range s.apps {
		for name, ch := range hub.channels {
			if len(ch.subs) == 0 && now.Sub(ch.last) > time.Hour {
				delete(hub.channels, name)
			}
		}
	}
}
