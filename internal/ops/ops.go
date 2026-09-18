// Package ops holds the daemon's app actions. The control socket the CLI talks to and the HTTP
// console are two transports over the same Service, so an action behaves the same however it is
// reached.
package ops

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"app-boss/internal/logstore"
	"app-boss/internal/notify"
	"app-boss/internal/super"
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
	ActionCron        = "cron"
	ActionCronRun     = "cron-run"
	ActionHook        = "hook"
	ActionHookRun     = "hook-run"
	ActionHookRotate  = "hook-rotate"
	ActionExec        = "exec"
	ActionAudit       = "audit"
	ActionLogSearch   = "log-search"
)

// auditActions are the methods that write an audit row when they run.
var auditActions = map[string]bool{
	ActionStart: true, ActionStop: true, ActionRestart: true, ActionMaintenance: true,
	ActionRescan: true, ActionCronRun: true, ActionHookRun: true, ActionHookRotate: true, ActionExec: true,
}

// Runtime is the supervisor surface the service drives.
type Runtime interface {
	Snapshots() []super.Snapshot
	Snapshot(name string) (super.Snapshot, error)
	Start(name string) error
	Stop(name string) error
	Restart(name string) error
	SetMaintenance(name string, on bool) error
	RunCron(name, job string) error
	RunHook(name, hook string) error
	RotateHook(name, hook string) (super.HookInfo, error)
	Hooks(name string) ([]super.HookInfo, error)
	HookSecret(name, hook string) (string, error)
	Exec(name string, argv []string, timeout time.Duration) (super.ExecResult, error)
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
	Tree(names []string) ([]logstore.AppTree, error)
}

// Auditor records and reads operator actions. logstore.Store implements it; a store that does not
// simply disables auditing.
type Auditor interface {
	RecordAudit(logstore.AuditEntry) error
	SearchAudit(logstore.AuditFilter) ([]logstore.AuditEntry, error)
}

// LatencyStore reads request duration quantiles; logstore.Store implements it.
type LatencyStore interface {
	Latency(app string, since time.Time) (logstore.Latency, error)
}

// Request is one action in transport-neutral form. The control socket decodes it from JSON and
// the console builds it from the HTTP body.
type Request struct {
	Method  string        `json:"method"`
	App     string        `json:"app,omitempty"`
	Process string        `json:"process,omitempty"`
	Job     string        `json:"job,omitempty"`
	Hook    string        `json:"hook,omitempty"`
	Argv    []string      `json:"argv,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`
	Lines   int           `json:"lines,omitempty"`
	On      bool          `json:"on,omitempty"`
	Actor   string        `json:"actor,omitempty"`
	Action  string        `json:"action,omitempty"`
	Query   string        `json:"query,omitempty"`
	Level   string        `json:"level,omitempty"`
	Channel string        `json:"channel,omitempty"`
}

// RescanResult is what a rescan changed: the fleet after the scan, apps it could not load and
// host keys that only apply on the next start.
type RescanResult struct {
	Apps            []super.Snapshot `json:"apps"`
	Invalid         []string         `json:"invalid"`
	RestartRequired []string         `json:"restart_required"`
}

// Service implements every action once. rates and store may be nil, then snapshots carry no
// request rates and the log viewer is unavailable. When store also implements Auditor, every
// mutating action is audited.
type Service struct {
	runtime Runtime
	rates   Rates
	store   LogStore
	auditor Auditor
	sink    notify.Sink
}

func New(runtime Runtime, rates Rates, store LogStore, sinks ...notify.Sink) *Service {
	service := &Service{runtime: runtime, rates: rates, store: store}
	if auditor, ok := store.(Auditor); ok {
		service.auditor = auditor
	}
	if len(sinks) > 0 {
		service.sink = sinks[0]
	}
	return service
}

// SearchLogs and SearchRequests are the read side of the log store, shared by the console and
// any future `appboss logs --search`.
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

// LogTree is the log viewer's left nav: every app, sqlite size on disk, and the channels inside.
func (s *Service) LogTree() ([]logstore.AppTree, error) {
	if s.store == nil {
		return nil, errors.New("log store is not enabled")
	}
	names := make([]string, 0, len(s.runtime.Snapshots())+1)
	for _, snapshot := range s.runtime.Snapshots() {
		names = append(names, snapshot.Name)
	}
	slices.Sort(names)
	names = append(names, logstore.HostApp)
	return s.store.Tree(names)
}

// Do runs one action by name. Both transports call it, so the name-to-method mapping and the
// audit row live here only.
func (s *Service) Do(request Request) (any, error) {
	result, err := s.dispatch(request)
	s.auditRequest(request, err)
	return result, err
}

func (s *Service) dispatch(request Request) (any, error) {
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
	case ActionCron:
		return s.Cron(request.App)
	case ActionCronRun:
		return nil, s.RunCron(request.App, request.Job)
	case ActionHook:
		return s.Hooks(request.App)
	case ActionHookRun:
		return nil, s.RunHook(request.App, request.Hook)
	case ActionHookRotate:
		return s.RotateHook(request.App, request.Hook)
	case ActionExec:
		return s.Exec(request.App, request.Argv, request.Timeout)
	case ActionAudit:
		return s.SearchAudit(auditFilter(request))
	case ActionLogSearch:
		return s.SearchLogs(request.App, logstore.LogFilter{Channel: request.Channel, Process: request.Process, Level: request.Level, Query: request.Query, Limit: request.Lines})
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownAction, request.Method)
	}
}

// SearchAudit returns audit rows for the console and CLI.
func (s *Service) SearchAudit(filter logstore.AuditFilter) ([]logstore.AuditEntry, error) {
	if s.auditor == nil {
		return nil, errors.New("audit is not enabled")
	}
	return s.auditor.SearchAudit(filter)
}

// Audit records one operator action that does not go through Do, such as a config file write.
func (s *Service) Audit(actor, app, action, detail string, err error) {
	if s.auditor == nil {
		return
	}
	if actor == "" {
		actor = "cli"
	}
	result, message := "ok", ""
	if err != nil {
		result, message = "error", err.Error()
	}
	_ = s.auditor.RecordAudit(logstore.AuditEntry{Time: time.Now(), Actor: actor, App: app, Action: action, Detail: detail, Result: result, Error: message})
}

// Notify sends one operator notification through the host webhook, for actions outside the
// supervisor (a config change that needs a restart).
func (s *Service) Notify(eventType, app, detail string) {
	if s.sink == nil {
		return
	}
	s.sink.Send(notify.Event{Type: eventType, App: app, Error: detail, Time: time.Now()})
}

func (s *Service) auditRequest(request Request, err error) {
	if !auditActions[request.Method] {
		return
	}
	s.Audit(request.Actor, request.App, request.Method, auditDetail(request), err)
}

func auditDetail(request Request) string {
	switch request.Method {
	case ActionMaintenance:
		if request.On {
			return "on"
		}
		return "off"
	case ActionCronRun, ActionHookRun, ActionHookRotate:
		if request.Job != "" {
			return request.Job
		}
		return request.Hook
	case ActionExec:
		return strings.Join(request.Argv, " ")
	default:
		return ""
	}
}

func auditFilter(request Request) logstore.AuditFilter {
	return logstore.AuditFilter{App: request.App, Actor: request.Actor, Action: request.Action, Limit: request.Lines}
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

// Cron lists the scheduled jobs of one app from its live snapshot.
func (s *Service) Cron(name string) ([]super.CronSnapshot, error) {
	snapshot, err := s.runtime.Snapshot(name)
	if err != nil {
		return nil, err
	}
	return snapshot.Cron, nil
}

// RunCron starts one scheduled job now.
func (s *Service) RunCron(name, job string) error { return s.runtime.RunCron(name, job) }

// Hooks lists an app's deploy hooks with their effective secrets.
func (s *Service) Hooks(name string) ([]super.HookInfo, error) { return s.runtime.Hooks(name) }

// RunHook starts one deploy hook now.
func (s *Service) RunHook(name, hook string) error { return s.runtime.RunHook(name, hook) }

// RotateHook mints a new generated secret for one hook and returns it with its URL.
func (s *Service) RotateHook(name, hook string) (super.HookInfo, error) {
	return s.runtime.RotateHook(name, hook)
}

// HookSecret returns the effective secret of one hook, for verifying a ping.
func (s *Service) HookSecret(name, hook string) (string, error) { return s.runtime.HookSecret(name, hook) }

// Exec runs one command in the app's environment and returns its combined output.
func (s *Service) Exec(name string, argv []string, timeout time.Duration) (super.ExecResult, error) {
	return s.runtime.Exec(name, argv, timeout)
}

// Latency returns request duration quantiles of one app over the last hour, for /metrics.
func (s *Service) Latency(name string) (logstore.Latency, error) {
	store, ok := s.store.(LatencyStore)
	if !ok {
		return logstore.Latency{}, errors.New("latency is not available")
	}
	return store.Latency(name, time.Now().Add(-time.Hour))
}

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
