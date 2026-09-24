package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/events"
	"dboss/internal/supervisor"
)

type memoryEvents struct {
	rows map[string][]events.Row
	err  error
}

func (m *memoryEvents) Append(_, ns string, rows []events.Row) error {
	if m.err != nil {
		return m.err
	}
	if m.rows == nil {
		m.rows = map[string][]events.Row{}
	}
	m.rows[ns] = append(m.rows[ns], rows...)
	return nil
}

func eventSnapshot(dir string) supervisor.Snapshot {
	snap := snapshot(dir)
	snap.Web.Events = config.Events{Retention: config.Duration(time.Hour)}
	return snap
}

func TestEventLogGoesToEventsNotLogs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "log", "billing"), 0o755)
	writeLog(t, filepath.Join(dir, "log", "billing", "invoice.json.log"), strings.Join([]string{
		`{"msg":"paid","tags":["plan:pro"],"data":{"event":"invoice_paid","request_id":"r1","value":9}}`,
		`not json`,
		``,
		`{"data":{"event":"invoice_sent"}}`,
		`{"data":{"event":"partial"`, // no newline yet: waits
	}, "\n"))
	sink := &memorySink{countries: map[string]string{"r1": "HR"}}
	sinkEvents := &memoryEvents{}
	module := New(&fakeSealer{}, fakeApps{[]supervisor.Snapshot{eventSnapshot(dir)}}, sink, sinkEvents, time.Second)
	module.runOnce()

	rows := sinkEvents.rows["billing.invoice"]
	if len(rows) != 2 || rows[0].Event != "invoice_paid" || rows[1].Event != "invoice_sent" {
		t.Fatalf("events = %+v", sinkEvents.rows)
	}
	if rows[0].Country != "HR" || rows[0].EID == 0 || rows[0].EID == rows[1].EID {
		t.Fatalf("row = %+v", rows[0])
	}
	if len(sink.entries) != 1 || sink.entries[0].Level != "warn" || !strings.Contains(sink.entries[0].Message, "not a JSON object") {
		t.Fatalf("log entries = %+v", sink.entries)
	}
	path := filepath.Join(dir, "log", "billing", "invoice.json.log")
	data, _ := os.ReadFile(path)
	if want := int64(strings.LastIndex(string(data), "\n") + 1); sink.offsets[path].Offset != want {
		t.Fatalf("offset = %d, want %d (before the partial line)", sink.offsets[path].Offset, want)
	}

	// The same pass again reads nothing new; the eid of a line is stable.
	module.runOnce()
	if len(sinkEvents.rows["billing.invoice"]) != 2 {
		t.Fatalf("re-read: %+v", sinkEvents.rows)
	}
}

func TestEventAppendFailureKeepsOffset(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "log"), 0o755)
	path := filepath.Join(dir, "log", "shop.json.log")
	writeLog(t, path, `{"data":{"event":"a"}}`+"\n")
	sink := &memorySink{}
	module := New(&fakeSealer{}, fakeApps{[]supervisor.Snapshot{eventSnapshot(dir)}}, sink, &memoryEvents{err: errors.New("disk full")}, time.Second)
	module.runOnce()
	if _, ok := sink.offsets[path]; ok {
		t.Fatal("offset advanced although the events were not stored")
	}
}

func TestEventsOffLeavesFileAlone(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "log"), 0o755)
	path := filepath.Join(dir, "log", "shop.json.log")
	writeLog(t, path, `{"data":{"event":"a"}}`+"\n")
	sink := &memorySink{}
	sinkEvents := &memoryEvents{}
	module := New(&fakeSealer{}, fakeApps{[]supervisor.Snapshot{snapshot(dir)}}, sink, sinkEvents, time.Second)
	module.runOnce()
	if len(sinkEvents.rows) != 0 || len(sink.entries) != 0 {
		t.Fatalf("events off still ingested: %+v %+v", sinkEvents.rows, sink.entries)
	}
	if _, ok := sink.offsets[path]; ok {
		t.Fatal("events off must not move the offset")
	}
}

func TestBadNamespaceIsSkipped(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "log"), 0o755)
	writeLog(t, filepath.Join(dir, "log", "Bad Name.json.log"), `{"data":{"event":"a"}}`+"\n")
	sinkEvents := &memoryEvents{}
	module := New(&fakeSealer{}, fakeApps{[]supervisor.Snapshot{eventSnapshot(dir)}}, &memorySink{}, sinkEvents, time.Second)
	module.runOnce()
	if len(sinkEvents.rows) != 0 {
		t.Fatalf("rows = %+v", sinkEvents.rows)
	}
}
