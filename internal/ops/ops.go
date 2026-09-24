// Package ops holds the daemon's app actions. The control socket the CLI talks to and the HTTP
// console are two transports over the same Service, so an action behaves the same however it is
// reached.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"dboss/internal/config"
	"dboss/internal/diskusage"
	"dboss/internal/events"
	"dboss/internal/logstore"
	"dboss/internal/notify"
	"dboss/internal/pg"
	"dboss/internal/pubsub"
	"dboss/internal/supervisor"
)

// ErrUnknownAction is returned by Do for a method it does not implement, so a transport can tell
// a bad request from an action that failed.
var ErrUnknownAction = errors.New("unknown action")

// Action names are the canonical method strings shared by the control protocol and the console.
const (
	ActionList          = "ls"
	ActionStatus        = "status"
	ActionStart         = "start"
	ActionStop          = "stop"
	ActionRestart       = "restart"
	ActionDestroy       = "destroy"
	ActionMaintenance   = "maintenance"
	ActionRescan        = "rescan"
	ActionLogs          = "logs"
	ActionPorts         = "ports"
	ActionCron          = "cron"
	ActionCronRun       = "cron-run"
	ActionHook          = "hook"
	ActionHookRun       = "hook-run"
	ActionHostHookRun   = "host-hook-run"
	ActionExec          = "exec"
	ActionAudit         = "audit"
	ActionLogSearch     = "log-search"
	ActionPG            = "pg"
	ActionPGBackup      = "pg-backup"
	ActionPGBackups     = "pg-backups"
	ActionPGRestore     = "pg-restore"
	ActionPGDrop        = "pg-drop"
	ActionPGDeleteDump  = "pg-delete-dump"
	ActionPGQuery       = "pg-query"
	ActionPubsub        = "pubsub"
	ActionPubsubSecret  = "pubsub-secret"
	ActionPubsubRotate  = "pubsub-rotate"
	ActionPubsubPublish = "pubsub-publish"
	ActionAdd           = "add"
	ActionEvents        = "events"
	ActionEventsLatest  = "events-latest"
	ActionEventsFacets  = "events-facets"
	ActionEventsViews   = "events-views"
	ActionEventsFunnel  = "events-funnel"
	ActionEventsQuery   = "events-query"
	ActionEventsSave    = "events-save"
	ActionEventsDelete  = "events-delete"
)

// auditActions are the methods that write an audit row when they run.
var auditActions = map[string]bool{
	ActionStart: true, ActionStop: true, ActionRestart: true, ActionDestroy: true, ActionMaintenance: true,
	ActionRescan: true, ActionCronRun: true, ActionHookRun: true, ActionHostHookRun: true, ActionExec: true,
	ActionPGBackup: true, ActionPGRestore: true, ActionPGDrop: true, ActionPGDeleteDump: true, ActionPGQuery: true,
	ActionPubsubRotate: true, ActionPubsubPublish: true, ActionAdd: true,
	ActionEventsQuery: true, ActionEventsSave: true, ActionEventsDelete: true,
}

// Runtime is the supervisor surface the service drives.
type Runtime interface {
	Snapshots() []supervisor.Snapshot
	Snapshot(name string) (supervisor.Snapshot, error)
	Start(name string) error
	Stop(name string) error
	Restart(name string) error
	StartProcess(name, process string) error
	StopProcess(name, process string) error
	RestartProcess(name, process string) error
	Destroy(name string) error
	SetMaintenance(name string, on bool) error
	RunCron(name, job string) error
	RunHook(name, hook string) error
	Hooks(name string) ([]supervisor.HookInfo, error)
	HookToken(name, hook string) (string, error)
	HostHookToken(name string) (string, error)
	Exec(name string, argv []string, timeout time.Duration) (supervisor.ExecResult, error)
	Rescan() ([]error, error)
	RestartRequired() []string
	Booted() bool
	HostConfig() config.Config
	Logs(name, process string, lines int) (map[string][]string, error)
	Ports() map[string]int
}

// LogStore is the per-app log database; logstore.Store is the only one. It carries the request
// rates, the log and request viewers, the Traffic tab, the metrics latency and the audit trail, so
// a nil store turns all of them off together.
type LogStore interface {
	Rates(app string) (logstore.Rates, error)
	Window(app string, since time.Time) (logstore.Window, error)
	Traffic(app string, since time.Time) (logstore.Traffic, error)
	Series(apps []string, since time.Time) ([]logstore.TrafficBucket, error)
	SearchLogs(app string, filter logstore.LogFilter) ([]logstore.LogEntry, error)
	SearchRequests(app string, filter logstore.RequestFilter) ([]logstore.RequestEntry, error)
	Channels(app string) ([]logstore.Channel, error)
	Tree(names []string) ([]logstore.AppTree, error)
	RecordAudit(logstore.AuditEntry) error
	SearchAudit(logstore.AuditFilter) ([]logstore.AuditEntry, error)
}

// Notifier is the operator webhook: notify.Notifier implements it.
type Notifier interface {
	Send(notify.Event)
	Stats() notify.Stats
}

// Disk reads what an app occupies on disk. diskusage.Module implements it; a nil value leaves
// every snapshot's Disk block zero.
type Disk interface {
	Usage(app string) (diskusage.Usage, bool)
	Refresh(app string) (diskusage.Usage, error)
}

// PG is the PostgreSQL inspection and backup surface. pg.Service implements it; a nil value
// disables the feature and every PG action answers with a clear error.
type PG interface {
	Enabled() bool
	Available() bool
	Snapshot() pg.Snapshot
	Refresh(ctx context.Context) pg.Snapshot
	BackupAll(ctx context.Context) error
	BackupDatabase(ctx context.Context, database string, manual bool) (pg.Backup, error)
	Backups() []pg.Backup
	BackupFile(id string) (pg.Backup, string, error)
	ImportBackup(database string, source io.Reader) (pg.Backup, error)
	DeleteBackup(id string) error
	Restore(ctx context.Context, request pg.RestoreRequest) (pg.RestoreResult, error)
	DropDatabase(ctx context.Context, database, confirm string) error
	Query(ctx context.Context, database, sql string) (pg.QueryResult, error)
	BackupConfig() config.PostgresBackups
	Apply(cfg config.Config)
}

// Pubsub is the realtime channel surface. pubsub.Service implements it; a nil value disables the
// feature and every pubsub action answers with a clear error.
type Pubsub interface {
	Secret(app, process string, cfg config.Pubsub) (string, error)
	Rotate(app, process string, cfg config.Pubsub) (string, error)
	Publish(app, process, channel string, msg pubsub.Message, replay int) int
	Snapshot(snapshots []supervisor.Snapshot) []pubsub.App
	Stats() map[string]pubsub.Stats
	Reconcile(snapshots []supervisor.Snapshot)
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
	// ByActor filters the audit search; Actor is who is asking.
	ByActor  string          `json:"by_actor,omitempty"`
	Action   string          `json:"action,omitempty"`
	Query    string          `json:"query,omitempty"`
	Level    string          `json:"level,omitempty"`
	Channel  string          `json:"channel,omitempty"`
	Event    string          `json:"event,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Database string          `json:"database,omitempty"`
	SQL      string          `json:"sql,omitempty"`
	BackupID string          `json:"backup_id,omitempty"`
	Target   string          `json:"target,omitempty"`
	Replace  bool            `json:"replace,omitempty"`
	Confirm  string          `json:"confirm,omitempty"`
	// Params carries the request query parameters a hook ping arrived with, keyed by their QS_
	// name. The built-in github_pr hook reads branch/repo/action/num from it.
	Params map[string]string `json:"params,omitempty"`
	// Repo, Branch and Host describe the app an add clones: the git URL, the branch (empty for
	// the default one) and a host that replaces the app's own.
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
	Host   string `json:"host,omitempty"`
	// Key, Kind and Name address the events surface: a facet key (plan, #, data.), a saved
	// entry's kind (view or funnel) and its name.
	Key  string `json:"key,omitempty"`
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
}

// RescanResult is what a rescan changed: the fleet after the scan, apps it could not load and
// host keys that only apply on the next start.
type RescanResult struct {
	Apps            []supervisor.Snapshot `json:"apps"`
	Invalid         []string              `json:"invalid"`
	RestartRequired []string              `json:"restart_required"`
}

// Service implements every action once. Every dependency but runtime may be nil, which turns
// its feature off: no store means no rates, log viewer or audit trail.
type Service struct {
	runtime  Runtime
	store    LogStore
	notifier Notifier
	pg       PG
	pubsub   Pubsub
	disk     Disk
	events   *events.Service
	// previews serializes the built-in github_pr deploys and adds per app, so two pushes to one
	// branch never race the same checkout while different apps deploy in parallel.
	previewMu    sync.Mutex
	previewLocks map[string]*sync.Mutex
}

func New(runtime Runtime, store LogStore, postgres PG, realtime Pubsub, sizes Disk, notifier Notifier) *Service {
	return &Service{runtime: runtime, store: store, notifier: notifier, pg: postgres, pubsub: realtime, disk: sizes}
}

// Do runs one action by name. Both transports call it, so the name-to-method mapping and the
// audit row live here only.
func (s *Service) Do(request Request) (any, error) {
	if request.Method == ActionAdd {
		request.App = addName(request)
	}
	result, err := s.dispatch(request)
	s.auditRequest(request, err)
	return result, err
}

func (s *Service) dispatch(request Request) (any, error) {
	switch request.Method {
	case ActionList:
		return s.Apps(), nil
	case ActionStatus:
		return s.app(request.App)
	case ActionStart:
		return nil, s.start(request.App, request.Process)
	case ActionStop:
		return nil, s.stop(request.App, request.Process)
	case ActionRestart:
		return nil, s.restart(request.App, request.Process)
	case ActionDestroy:
		return nil, s.destroy(request.App)
	case ActionMaintenance:
		return nil, s.maintenance(request.App, request.On)
	case ActionRescan:
		return s.rescan()
	case ActionLogs:
		return s.logs(request.App, request.Process, request.Lines)
	case ActionPorts:
		return s.ports(), nil
	case ActionCron:
		return s.cron(request.App)
	case ActionCronRun:
		return nil, s.runCron(request.App, request.Job)
	case ActionHook:
		return s.Hooks(request.App)
	case ActionHookRun:
		return nil, s.runHook(request.App, request.Hook)
	case ActionHostHookRun:
		return nil, s.runHostHook(request.Hook, request.Params)
	case ActionExec:
		return s.exec(request.App, request.Argv, request.Timeout)
	case ActionAudit:
		return s.SearchAudit(auditFilter(request))
	case ActionLogSearch:
		return s.SearchLogs(request.App, logstore.LogFilter{Channel: request.Channel, Process: request.Process, Level: request.Level, Query: request.Query, Limit: request.Lines})
	case ActionPG:
		return s.PGSnapshot(true)
	case ActionPGBackup:
		return s.runBackup(request.Database)
	case ActionPGBackups:
		return s.Backups(), nil
	case ActionPGRestore:
		return s.restore(pg.RestoreRequest{ID: request.BackupID, Target: request.Target, Replace: request.Replace, Confirm: request.Confirm})
	case ActionPGDrop:
		return request.Database, s.dropDatabase(request.Database, request.Confirm)
	case ActionPGDeleteDump:
		return request.BackupID, s.deleteBackup(request.BackupID)
	case ActionPGQuery:
		return s.runQuery(request.Database, request.SQL)
	case ActionPubsub:
		return s.PubsubApps(), nil
	case ActionPubsubSecret:
		return s.PubsubSecret(request.App, request.Process)
	case ActionPubsubRotate:
		return s.pubsubRotate(request.App, request.Process)
	case ActionPubsubPublish:
		return s.pubsubPublish(request.App, request.Process, request.Channel, request.Event, request.Data)
	case ActionAdd:
		return s.add(request)
	case ActionEvents:
		return s.EventSummary(request.App, request.Query)
	case ActionEventsLatest:
		return s.LatestEvents(request.App, request.Query, request.Lines)
	case ActionEventsFacets:
		return s.EventFacets(request.App, request.Query, request.Key)
	case ActionEventsViews:
		return s.EventViews(request.App)
	case ActionEventsFunnel:
		return s.RunFunnel(request.App, request.Name, request.Data, request.Query)
	case ActionEventsQuery:
		return s.eventsQuery(request.App, request.SQL)
	case ActionEventsSave:
		return nil, s.eventsSave(request.App, request.Kind, request.Data)
	case ActionEventsDelete:
		return nil, s.eventsDelete(request.App, request.Kind, request.Name)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownAction, request.Method)
	}
}

// SearchAudit returns audit rows for the console and CLI.
func (s *Service) SearchAudit(filter logstore.AuditFilter) ([]logstore.AuditEntry, error) {
	if s.store == nil {
		return nil, errors.New("audit is not enabled")
	}
	return s.store.SearchAudit(filter)
}

// Audit records one operator action that does not go through Do, such as a config file write.
func (s *Service) Audit(actor, app, action, detail string, err error) {
	if s.store == nil {
		return
	}
	if actor == "" {
		actor = "cli"
	}
	result, message := "ok", ""
	if err != nil {
		result, message = "error", err.Error()
	}
	_ = s.store.RecordAudit(logstore.AuditEntry{Time: time.Now(), Actor: actor, App: app, Action: action, Detail: detail, Result: result, Error: message})
}

// notify sends one operator notification through the host webhook, for actions outside the
// supervisor (a config change that needs a restart).
func (s *Service) notify(eventType, app, detail string) {
	if s.notifier == nil {
		return
	}
	s.notifier.Send(notify.Event{Type: eventType, App: app, Error: detail, Time: time.Now()})
}

func (s *Service) auditRequest(request Request, err error) {
	if !auditActions[request.Method] {
		return
	}
	s.Audit(request.Actor, request.App, request.Method, auditDetail(request), err)
}

func auditDetail(request Request) string {
	switch request.Method {
	case ActionStart, ActionStop, ActionRestart:
		return request.Process
	case ActionMaintenance:
		if request.On {
			return "on"
		}
		return "off"
	case ActionCronRun, ActionHookRun:
		if request.Job != "" {
			return request.Job
		}
		return request.Hook
	case ActionExec:
		return strings.Join(request.Argv, " ")
	case ActionPGBackup:
		return request.Database
	case ActionPGRestore:
		return request.BackupID
	case ActionPGDrop:
		return request.Database
	case ActionPGDeleteDump:
		return request.BackupID
	case ActionPGQuery:
		return request.Database + ": " + pg.QueryAuditDetail(request.SQL)
	case ActionPubsubPublish:
		return request.Channel
	case ActionEventsQuery:
		return pg.QueryAuditDetail(request.SQL)
	case ActionEventsSave:
		return request.Kind + " " + savedName(request.Data)
	case ActionEventsDelete:
		return request.Kind + " " + request.Name
	case ActionAdd:
		detail := request.Repo
		if request.Branch != "" {
			detail += " branch=" + request.Branch
		}
		if request.Host != "" {
			detail += " host=" + request.Host
		}
		return detail
	default:
		return ""
	}
}

func auditFilter(request Request) logstore.AuditFilter {
	return logstore.AuditFilter{App: request.App, Actor: request.ByActor, Action: request.Action, Limit: request.Lines}
}
