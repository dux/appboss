package ops

import (
	"testing"

	"dboss/internal/logstore"
	"dboss/internal/notify"
)

type auditStore struct {
	rows []logstore.AuditEntry
}

func (s *auditStore) SearchLogs(string, logstore.LogFilter) ([]logstore.LogEntry, error) {
	return nil, nil
}
func (s *auditStore) SearchRequests(string, logstore.RequestFilter) ([]logstore.RequestEntry, error) {
	return nil, nil
}
func (s *auditStore) Channels(string) ([]logstore.Channel, error) { return nil, nil }
func (s *auditStore) Tree([]string) ([]logstore.AppTree, error)   { return nil, nil }
func (s *auditStore) RecordAudit(e logstore.AuditEntry) error     { s.rows = append(s.rows, e); return nil }
func (s *auditStore) SearchAudit(logstore.AuditFilter) ([]logstore.AuditEntry, error) {
	return s.rows, nil
}

func TestDoAuditsMutatingActions(t *testing.T) {
	store := &auditStore{}
	service := New(&fakeRuntime{}, nil, store, nil, nil)
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

func TestAuditDisabledWithoutAuditor(t *testing.T) {
	service := New(&fakeRuntime{}, nil, nil, nil, nil)
	if _, err := service.SearchAudit(logstore.AuditFilter{}); err == nil {
		t.Fatal("SearchAudit should fail without an auditor")
	}
	service.Audit("cli", "app", "restart", "", nil) // must not panic
}

type sinkRecorder struct{ events []notify.Event }

func (s *sinkRecorder) Send(event notify.Event) { s.events = append(s.events, event) }

func TestNotifyForwardsToSink(t *testing.T) {
	sink := &sinkRecorder{}
	service := New(&fakeRuntime{}, nil, nil, nil, nil, sink)
	service.Notify("config-changed", "web", "restart required: management")
	if len(sink.events) != 1 || sink.events[0].Type != "config-changed" || sink.events[0].App != "web" {
		t.Fatalf("events = %+v", sink.events)
	}
	service.Notify("config-changed", "web", "again")
	if len(sink.events) != 2 {
		t.Fatalf("second notify dropped: %+v", sink.events)
	}
}
