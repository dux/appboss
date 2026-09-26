package ops

import (
	"errors"
	"testing"

	"dboss/internal/apps"
	"dboss/internal/logstore"
	"dboss/internal/notify"
)

// auditStore records audit rows; any store call it does not answer panics on the nil LogStore.
type auditStore struct {
	LogStore
	rows []logstore.AuditEntry
}

func (s *auditStore) Rates(string) (logstore.Rates, error) { return logstore.Rates{}, nil }

func (s *auditStore) Channels(string) ([]logstore.Channel, error)     { return nil, nil }
func (s *auditStore) Tree([]string) ([]logstore.AppTree, error)       { return nil, nil }
func (s *auditStore) SetExceptionResolved(string, string, bool) error { return nil }
func (s *auditStore) SetExceptionIgnored(string, string, bool) error  { return nil }
func (s *auditStore) RecordAudit(e logstore.AuditEntry) error         { s.rows = append(s.rows, e); return nil }
func (s *auditStore) SearchAudit(logstore.AuditFilter) ([]logstore.AuditEntry, error) {
	return s.rows, nil
}

func TestDoAuditsMutatingActions(t *testing.T) {
	store := &auditStore{}
	service := New(&fakeRuntime{}, store, nil, nil, nil, nil)
	if _, err := service.Do(Request{Method: ActionRestart, App: "web", Actor: "admin@example.com"}); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 1 || store.rows[0].Action != "restart" || store.rows[0].Actor != "admin@example.com" || store.rows[0].Result != "ok" {
		t.Fatalf("audit = %+v", store.rows)
	}
	if _, err := service.Do(Request{Method: ActionList}); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 1 {
		t.Fatalf("read action was audited: %+v", store.rows)
	}
	service.Audit("", "app", "config-write", "app:x", nil)
	if len(store.rows) != 2 || store.rows[1].Actor != "cli" {
		t.Fatalf("explicit audit = %+v", store.rows)
	}
}

func TestDoAuditsExceptionResolve(t *testing.T) {
	store := &auditStore{}
	service := New(&fakeRuntime{}, store, nil, nil, nil, nil)
	if _, err := service.Do(Request{Method: ActionExceptionResolve, App: "web", ExpUID: "abc", On: true, Actor: "admin@example.com"}); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 1 || store.rows[0].Action != ActionExceptionResolve || store.rows[0].Detail != "abc" || store.rows[0].Result != "ok" {
		t.Fatalf("audit = %+v", store.rows)
	}
	if _, err := service.Do(Request{Method: ActionExceptionIgnore, App: "web", ExpUID: "abc", On: true, Actor: "admin@example.com"}); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 2 || store.rows[1].Action != ActionExceptionIgnore || store.rows[1].Detail != "abc" || store.rows[1].Result != "ok" {
		t.Fatalf("ignore audit = %+v", store.rows)
	}
}

func TestAuditDisabledWithoutAuditor(t *testing.T) {
	service := New(&fakeRuntime{}, nil, nil, nil, nil, nil)
	if _, err := service.SearchAudit(logstore.AuditFilter{}); err == nil {
		t.Fatal("SearchAudit should fail without an auditor")
	}
	service.Audit("cli", "app", "restart", "", nil) // must not panic
}

type sinkRecorder struct{ events []notify.Event }

func (s *sinkRecorder) Send(event notify.Event) { s.events = append(s.events, event) }
func (s *sinkRecorder) Stats() notify.Stats     { return notify.Stats{Sent: int64(len(s.events))} }

func TestNotifyForwardsToSink(t *testing.T) {
	sink := &sinkRecorder{}
	service := New(&fakeRuntime{}, nil, nil, nil, nil, sink)
	service.notify("config-changed", "web", "restart required: management")
	if len(sink.events) != 1 || sink.events[0].Type != "config-changed" || sink.events[0].App != "web" {
		t.Fatalf("events = %+v", sink.events)
	}
	service.notify("config-changed", "web", "again")
	if len(sink.events) != 2 {
		t.Fatalf("second notify dropped: %+v", sink.events)
	}
}

func TestSaveConfigAuditsRescansAndNotifies(t *testing.T) {
	store := &auditStore{}
	sink := &sinkRecorder{}
	runtime := &fakeRuntime{restart: []string{"proxy"}}
	service := New(runtime, store, nil, nil, nil, sink)

	result, err := service.SaveConfig("admin@example.com", "config-write", "app:web", func() (apps.ConfigFile, error) {
		return apps.ConfigFile{ID: "app:web", App: "web"}, nil
	})
	if err != nil || result.File.ID != "app:web" || len(result.RestartRequired) != 1 {
		t.Fatalf("SaveConfig = %+v, %v", result, err)
	}
	if runtime.actions[len(runtime.actions)-1] != "rescan" {
		t.Fatalf("no rescan after the write: %v", runtime.actions)
	}
	if len(store.rows) != 1 || store.rows[0].Action != "config-write" || store.rows[0].App != "web" || store.rows[0].Result != "ok" {
		t.Fatalf("audit = %+v", store.rows)
	}
	if len(sink.events) != 1 || sink.events[0].Type != notify.ConfigChanged {
		t.Fatalf("events = %+v", sink.events)
	}

	refused := errors.New("invalid yaml")
	if _, err := service.SaveConfig("admin@example.com", "config-write", "app:web", func() (apps.ConfigFile, error) {
		return apps.ConfigFile{}, refused
	}); !errors.Is(err, refused) {
		t.Fatalf("write error = %v", err)
	}
	if len(store.rows) != 2 || store.rows[1].Result != "error" || runtime.actions[len(runtime.actions)-1] != "rescan" || len(runtime.actions) != 1 {
		t.Fatalf("a failed write must audit and must not rescan: audit=%+v actions=%v", store.rows, runtime.actions)
	}
}
