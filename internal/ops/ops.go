// Package ops holds the daemon's app actions. The control socket the CLI talks to and the HTTP
// console are two transports over the same Service, so an action behaves the same however it is
// reached.
package ops

import (
	"errors"
	"fmt"

	"deploy-boss/internal/logstore"
	"deploy-boss/internal/super"
)

// ErrUnknownAction is returned by Do for a method it does not implement, so a transport can tell
// a bad request from an action that failed.
var ErrUnknownAction = errors.New("unknown action")

// Action names are the canonical method strings shared by the control protocol and the console.
const (
	ActionList        = "ls"
	ActionStatus      = "status"
	ActionStart       = "start"
	ActionStop        = "stop"
	ActionRestart     = "restart"
	ActionMaintenance = "maintenance"
	ActionRescan      = "rescan"
	ActionLogs        = "logs"
	ActionPorts       = "ports"
)

// Runtime is the supervisor surface the service drives.
type Runtime interface {
	Snapshots() []super.Snapshot
	Snapshot(name string) (super.Snapshot, error)
	Start(name string) error
	Stop(name string) error
	Restart(name string) error
	SetMaintenance(name string, on bool) error
	Rescan() ([]error, error)
	RestartRequired() []string
	Logs(name, process string, lines int) (map[string][]string, error)
	Ports() map[string]int
}

// Rates reads how many requests an app has answered, for snapshots to carry.
type Rates interface {
	Rates(app string) (logstore.Rates, error)
}

// LogStore reads the per-app log database for the console and CLI viewers.
type LogStore interface {
	SearchLogs(app string, filter logstore.LogFilter) ([]logstore.LogEntry, error)
	SearchRequests(app string, filter logstore.RequestFilter) ([]logstore.RequestEntry, error)
	Channels(app string) ([]logstore.Channel, error)
}

// Request is one action in transport-neutral form. The control socket decodes it from JSON and
// the console builds it from the HTTP body.
type Request struct {
	Method  string `json:"method"`
	App     string `json:"app,omitempty"`
	Process string `json:"process,omitempty"`
	Lines   int    `json:"lines,omitempty"`
	On      bool   `json:"on,omitempty"`
}

// RescanResult is what a rescan changed: the fleet after the scan, apps it could not load and
// host keys that only apply on the next start.
type RescanResult struct {
	Apps            []super.Snapshot `json:"apps"`
	Invalid         []string         `json:"invalid"`
	RestartRequired []string         `json:"restart_required"`
}

// Service implements every action once. rates and store may be nil, then snapshots carry no
// request rates and the log viewer is unavailable.
type Service struct {
	runtime Runtime
	rates   Rates
	store   LogStore
}

func New(runtime Runtime, rates Rates, store LogStore) *Service {
	return &Service{runtime: runtime, rates: rates, store: store}
}

// SearchLogs and SearchRequests are the read side of the log store, shared by the console and
// any future `dboss logs --search`.
func (s *Service) SearchLogs(app string, filter logstore.LogFilter) ([]logstore.LogEntry, error) {
	if s.store == nil {
		return nil, errors.New("log store is not enabled")
	}
	return s.store.SearchLogs(app, filter)
}

func (s *Service) SearchRequests(app string, filter logstore.RequestFilter) ([]logstore.RequestEntry, error) {
	if s.store == nil {
		return nil, errors.New("log store is not enabled")
	}
	return s.store.SearchRequests(app, filter)
}

// Channels lists the log types an app has for the console viewer.
func (s *Service) Channels(app string) ([]logstore.Channel, error) {
	if s.store == nil {
		return nil, errors.New("log store is not enabled")
	}
	return s.store.Channels(app)
}

// Do runs one action by name. Both transports call it, so the name-to-method mapping lives here
// only.
func (s *Service) Do(request Request) (any, error) {
	switch request.Method {
	case ActionList:
		return s.Apps(), nil
	case ActionStatus:
		return s.App(request.App)
	case ActionStart:
		return nil, s.Start(request.App)
	case ActionStop:
		return nil, s.Stop(request.App)
	case ActionRestart:
		return nil, s.Restart(request.App)
	case ActionMaintenance:
		return nil, s.Maintenance(request.App, request.On)
	case ActionRescan:
		return s.Rescan()
	case ActionLogs:
		return s.Logs(request.App, request.Process, request.Lines)
	case ActionPorts:
		return s.Ports(), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownAction, request.Method)
	}
}

// Apps returns every app snapshot with its request rates filled in.
func (s *Service) Apps() []super.Snapshot {
	snapshots := s.runtime.Snapshots()
	if s.rates == nil {
		return snapshots
	}
	for i := range snapshots {
		s.withRates(&snapshots[i])
	}
	return snapshots
}

func (s *Service) App(name string) (super.Snapshot, error) {
	snapshot, err := s.runtime.Snapshot(name)
	if err != nil {
		return snapshot, err
	}
	s.withRates(&snapshot)
	return snapshot, nil
}

func (s *Service) Start(name string) error   { return s.runtime.Start(name) }
func (s *Service) Stop(name string) error    { return s.runtime.Stop(name) }
func (s *Service) Restart(name string) error { return s.runtime.Restart(name) }

func (s *Service) Maintenance(name string, on bool) error {
	return s.runtime.SetMaintenance(name, on)
}

func (s *Service) Logs(name, process string, lines int) (map[string][]string, error) {
	return s.runtime.Logs(name, process, lines)
}

func (s *Service) Ports() map[string]int { return s.runtime.Ports() }

// RestartRequired lists the host keys whose value on disk differs from the running session.
func (s *Service) RestartRequired() []string { return s.runtime.RestartRequired() }

// Rescan reloads the apps and reports the invalid ones and the host keys waiting for a restart,
// so every transport answers with the same shape.
func (s *Service) Rescan() (RescanResult, error) {
	invalid, err := s.runtime.Rescan()
	result := RescanResult{Apps: s.Apps(), Invalid: messages(invalid), RestartRequired: s.runtime.RestartRequired()}
	return result, err
}

func (s *Service) withRates(snapshot *super.Snapshot) {
	rates, err := s.rates.Rates(snapshot.Name)
	if err != nil {
		return
	}
	snapshot.RequestRates = super.RequestRates{LastMinute: rates.LastMinute, LastHour: rates.LastHour, LastDay: rates.LastDay}
}

func messages(failures []error) []string {
	result := make([]string, len(failures))
	for i, err := range failures {
		result[i] = err.Error()
	}
	return result
}
