package ops

import (
	"encoding/json"
	"errors"
	"fmt"

	"dboss/internal/config"
	"dboss/internal/pubsub"
	"dboss/internal/supervisor"
)

// PubsubApps lists every app that serves realtime channels, with its channels and subscriber
// counts.
func (s *Service) PubsubApps() []pubsub.App {
	if s.pubsub == nil {
		return nil
	}
	return s.pubsub.Snapshot(s.runtime.Snapshots())
}

// pubsubStats is the per-app realtime counters for /metrics.
func (s *Service) pubsubStats() map[string]pubsub.Stats {
	if s.pubsub == nil {
		return nil
	}
	return s.pubsub.Stats()
}

// PubsubSecret is a web process's publish credential and the URLs it enables, for the CLI and
// console.
type PubsubSecret struct {
	App       string `json:"app"`
	Process   string `json:"process"`
	Path      string `json:"path"`
	Host      string `json:"host"`
	Secret    string `json:"secret"`
	Subscribe string `json:"subscribe_url"`
	Publish   string `json:"publish_url"`
}

// PubsubSecret returns a web process's effective publish secret and its example URLs.
func (s *Service) PubsubSecret(app, process string) (PubsubSecret, error) {
	snapshot, web, err := s.pubsubHub(app, process)
	if err != nil {
		return PubsubSecret{}, err
	}
	cfg := web.Pubsub
	secret, err := s.pubsub.Secret(snapshot.Name, web.Name, cfg)
	if err != nil {
		return PubsubSecret{}, err
	}
	// A process with only "*." patterns has no single host, so the example shows the pattern.
	host := config.PrimaryHost(web.CanonicalHost, web.Hosts)
	if host == "" && len(web.Hosts) > 0 {
		host = web.Hosts[0]
	}
	base := "https://" + host + cfg.Path
	return PubsubSecret{App: snapshot.Name, Process: web.Name, Path: cfg.Path, Host: host, Secret: secret, Subscribe: base + "/<channel>", Publish: base + "/<channel>"}, nil
}

// pubsubRotate replaces a web process's generated publish secret and returns the new credential
// and URLs.
func (s *Service) pubsubRotate(app, process string) (PubsubSecret, error) {
	snapshot, web, err := s.pubsubHub(app, process)
	if err != nil {
		return PubsubSecret{}, err
	}
	if _, err := s.pubsub.Rotate(snapshot.Name, web.Name, web.Pubsub); err != nil {
		return PubsubSecret{}, err
	}
	return s.PubsubSecret(app, web.Name)
}

// PubsubPublished is the result of a console or CLI publish.
type PubsubPublished struct {
	Channel     string `json:"channel"`
	Subscribers int    `json:"subscribers"`
}

// pubsubPublish sends one message to a web process's channel from the CLI or console.
func (s *Service) pubsubPublish(app, process, channel, event string, data json.RawMessage) (PubsubPublished, error) {
	snapshot, web, err := s.pubsubHub(app, process)
	if err != nil {
		return PubsubPublished{}, err
	}
	if !pubsub.ValidChannel(channel) {
		return PubsubPublished{}, fmt.Errorf("invalid channel %q", channel)
	}
	if event == "" {
		event = "message"
	}
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	delivered := s.pubsub.Publish(snapshot.Name, web.Name, channel, pubsub.Message{Event: event, Data: data}, web.Pubsub.Replay)
	return PubsubPublished{Channel: channel, Subscribers: delivered}, nil
}

// pubsubHub resolves the web process a pubsub action targets. An empty process picks the app's
// only hub; an app with several hubs requires naming one.
func (s *Service) pubsubHub(app, process string) (supervisor.Snapshot, supervisor.WebProcessSnapshot, error) {
	if s.pubsub == nil {
		return supervisor.Snapshot{}, supervisor.WebProcessSnapshot{}, errors.New("pubsub is not enabled")
	}
	if app == "" {
		return supervisor.Snapshot{}, supervisor.WebProcessSnapshot{}, errors.New("app is required")
	}
	snapshot, err := s.runtime.Snapshot(app)
	if err != nil {
		return supervisor.Snapshot{}, supervisor.WebProcessSnapshot{}, err
	}
	var enabled []supervisor.WebProcessSnapshot
	for _, web := range snapshot.WebProcesses {
		if web.Pubsub.Enabled() {
			enabled = append(enabled, web)
		}
	}
	if len(enabled) == 0 {
		return supervisor.Snapshot{}, supervisor.WebProcessSnapshot{}, fmt.Errorf("app %q has no pubsub path", app)
	}
	if process == "" {
		if len(enabled) > 1 {
			names := make([]string, len(enabled))
			for index, web := range enabled {
				names[index] = web.Name
			}
			return supervisor.Snapshot{}, supervisor.WebProcessSnapshot{}, fmt.Errorf("app %q has several pubsub processes %v; name one", app, names)
		}
		return snapshot, enabled[0], nil
	}
	for _, web := range enabled {
		if web.Name == process {
			return snapshot, web, nil
		}
	}
	return supervisor.Snapshot{}, supervisor.WebProcessSnapshot{}, fmt.Errorf("app %q web process %q has no pubsub path", app, process)
}
