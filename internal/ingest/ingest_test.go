package ingest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"deploy-boss/internal/logstore"
	"deploy-boss/internal/super"
)

func TestParseLineReadsJSONAndPlain(t *testing.T) {
	entry := ParseLine("web", `{"level":"error","message":"boom","request_id":"r1"}`)
	if entry.Level != "error" || entry.Message != "boom" || entry.RequestID != "r1" || entry.Process != "web" {
		t.Fatalf("unexpected structured entry: %+v", entry)
	}
	plain := ParseLine("worker", "some ERROR happened")
	if plain.Level != "error" || plain.Message != "some ERROR happened" || plain.Raw != "some ERROR happened" {
		t.Fatalf("unexpected plain entry: %+v", plain)
	}
}

func TestParseFileNamesTheProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.log.1234.sealed")
	if err := os.WriteFile(path, []byte("one\n{\"level\":\"warn\",\"msg\":\"two\"}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	entries, err := ParseFile(path, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Process != "web" || entries[1].Level != "warn" || entries[1].Message != "two" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

type fakeSealer struct{ paths []string }

func (f *fakeSealer) SealLogs(string) ([]string, error) { return f.paths, nil }

type fakeApps struct{}

func (fakeApps) Snapshots() []super.Snapshot { return []super.Snapshot{{Name: "demo"}} }

type memorySink struct{ entries []logstore.LogEntry }

func (m *memorySink) RecordLogs(_ string, entries []logstore.LogEntry) error {
	m.entries = append(m.entries, entries...)
	return nil
}

func TestRunOnceIngestsThenDeletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.log.1.sealed")
	if err := os.WriteFile(path, []byte("hello\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	module := New(&fakeSealer{paths: []string{path}}, fakeApps{}, sink, time.Second)
	module.runOnce()
	if len(sink.entries) != 1 || sink.entries[0].Message != "hello" {
		t.Fatalf("unexpected sink entries: %+v", sink.entries)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("sealed segment should be removed after ingest: %v", err)
	}
}
