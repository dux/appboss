package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dboss/internal/logstore"
	"dboss/internal/super"
)

func TestParseLineReadsJSONAndPlain(t *testing.T) {
	entry := ParseLine("stdout", "web", `{"level":"error","message":"boom","request_id":"r1"}`)
	if entry.Level != "error" || entry.Message != "boom" || entry.RequestID != "r1" || entry.Process != "web" || entry.Source != "stdout" {
		t.Fatalf("unexpected structured entry: %+v", entry)
	}
	plain := ParseLine("file", "production.log", "some ERROR happened")
	if plain.Source != "file" || plain.Process != "production.log" || plain.Level != "error" || plain.Message != "some ERROR happened" || plain.Raw != "some ERROR happened" {
		t.Fatalf("unexpected plain entry: %+v", plain)
	}
}

func TestParseFileNamesTheProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.log.1234.sealed")
	if err := os.WriteFile(path, []byte("one\n{\"level\":\"warn\",\"msg\":\"two\"}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	entries, err := ParseFile(path, "stdout")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Process != "web" || entries[0].Source != "stdout" || entries[1].Level != "warn" || entries[1].Message != "two" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

type fakeSealer struct{ paths []string }

func (f *fakeSealer) SealLogs(string) ([]string, error) { return f.paths, nil }

type fakeApps struct{ snapshots []super.Snapshot }

func (f fakeApps) Snapshots() []super.Snapshot { return f.snapshots }

type memorySink struct {
	entries   []logstore.LogEntry
	offsets   map[string]logstore.TailOffset
	removed   []string
	appendErr error
}

func (m *memorySink) RecordLogs(_ string, entries []logstore.LogEntry) error {
	m.entries = append(m.entries, entries...)
	return nil
}

func (m *memorySink) AppendLogs(_ string, entries []logstore.LogEntry) error {
	if m.appendErr != nil {
		return m.appendErr
	}
	m.entries = append(m.entries, entries...)
	return nil
}

func (m *memorySink) TailOffsets(string) (map[string]logstore.TailOffset, error) {
	if m.offsets == nil {
		m.offsets = map[string]logstore.TailOffset{}
	}
	return m.offsets, nil
}

func (m *memorySink) SaveTailOffset(_, path string, inode uint64, offset int64) error {
	if m.offsets == nil {
		m.offsets = map[string]logstore.TailOffset{}
	}
	m.offsets[path] = logstore.TailOffset{Path: path, Inode: inode, Offset: offset}
	return nil
}

func (m *memorySink) RemoveTailOffsets(_ string, paths []string) error {
	m.removed = append(m.removed, paths...)
	for _, path := range paths {
		delete(m.offsets, path)
	}
	return nil
}

func snapshot(dir string) super.Snapshot {
	return super.Snapshot{Name: "demo", Dir: dir, LogRetention: time.Hour, StdoutRetention: time.Hour}
}

func TestRunOnceIngestsSealedStdoutThenDeletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.log.1.sealed")
	if err := os.WriteFile(path, []byte("hello\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	module := New(&fakeSealer{paths: []string{path}}, fakeApps{[]super.Snapshot{snapshot(t.TempDir())}}, sink, time.Second)
	module.runOnce()
	if len(sink.entries) != 1 || sink.entries[0].Message != "hello" || sink.entries[0].Source != "stdout" {
		t.Fatalf("unexpected sink entries: %+v", sink.entries)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("sealed segment should be removed after ingest: %v", err)
	}
}

func TestCommitFailureKeepsSegmentAndOffset(t *testing.T) {
	segment := filepath.Join(t.TempDir(), "web.log.1.sealed")
	if err := os.WriteFile(segment, []byte("hello\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "production.log")
	if err := os.WriteFile(path, []byte("one\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{appendErr: errors.New("database unavailable")}
	module := New(&fakeSealer{paths: []string{segment}}, fakeApps{[]super.Snapshot{snapshot(dir)}}, sink, time.Second)
	module.runOnce()

	if _, err := os.Stat(segment); err != nil {
		t.Fatalf("a segment must survive a failed commit: %v", err)
	}
	if sink.offsets[path].Offset != 0 {
		t.Fatalf("offset must not advance on a failed commit: %+v", sink.offsets[path])
	}
}

func TestTailFileReadsOnlyNewBytes(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "production.log")
	if err := os.WriteFile(path, []byte("one\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	module := New(&fakeSealer{}, fakeApps{[]super.Snapshot{snapshot(dir)}}, sink, time.Second)

	module.runOnce()
	if len(sink.entries) != 1 || sink.entries[0].Message != "one" || sink.entries[0].Source != "file" || sink.entries[0].Process != "production.log" {
		t.Fatalf("unexpected first pass: %+v", sink.entries)
	}
	if sink.offsets[path].Offset != int64(len("one\n")) {
		t.Fatalf("offset not committed: %+v", sink.offsets[path])
	}

	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	module.runOnce()
	if len(sink.entries) != 2 || sink.entries[1].Message != "two" {
		t.Fatalf("second pass should read only the new line: %+v", sink.entries)
	}
}

func TestTailHoldsBackPartialLine(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "app.log")
	if err := os.WriteFile(path, []byte("full\npart"), 0o640); err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	module := New(&fakeSealer{}, fakeApps{[]super.Snapshot{snapshot(dir)}}, sink, time.Second)

	module.runOnce()
	if len(sink.entries) != 1 || sink.entries[0].Message != "full" {
		t.Fatalf("partial line should wait: %+v", sink.entries)
	}
	if err := os.WriteFile(path, []byte("full\npartial\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	module.runOnce()
	if len(sink.entries) != 2 || sink.entries[1].Message != "partial" {
		t.Fatalf("second pass should read the completed line: %+v", sink.entries)
	}
}

func TestTailResetsOnTruncation(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "app.log")
	if err := os.WriteFile(path, []byte("long enough first line\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	module := New(&fakeSealer{}, fakeApps{[]super.Snapshot{snapshot(dir)}}, sink, time.Second)
	module.runOnce()

	if err := os.WriteFile(path, []byte("new\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	module.runOnce()
	if len(sink.entries) != 2 || sink.entries[1].Message != "new" {
		t.Fatalf("truncated file should restart: %+v", sink.entries)
	}
}

func TestTailDropsStaleOffsets(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o750); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(dir, "log", "gone.log")
	sink := &memorySink{offsets: map[string]logstore.TailOffset{gone: {Path: gone, Offset: 10}}}
	module := New(&fakeSealer{}, fakeApps{[]super.Snapshot{snapshot(dir)}}, sink, time.Second)
	module.runOnce()
	if len(sink.removed) != 1 || sink.removed[0] != gone {
		t.Fatalf("stale offset should be removed: %+v", sink.removed)
	}
}

func TestDaemonSinkBuffersLines(t *testing.T) {
	sink := &memorySink{}
	daemon := NewDaemonSink(sink)
	if _, err := daemon.Write([]byte("started\npartial")); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.Write([]byte(" done\n")); err != nil {
		t.Fatal(err)
	}
	if len(sink.entries) != 2 || sink.entries[0].Source != "dboss" || sink.entries[1].Message != "partial done" {
		t.Fatalf("unexpected daemon entries: %+v", sink.entries)
	}
}
