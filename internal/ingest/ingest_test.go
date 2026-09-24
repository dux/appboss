package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dboss/internal/logstore"
	"dboss/internal/supervisor"
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

func TestParseFilesNamesTheProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.log.1234.sealed")
	writeLog(t, path, "one\n{\"level\":\"warn\",\"msg\":\"two\"}\n")
	group, err := parseFiles([]string{path}, "stdout")
	if err != nil {
		t.Fatal(err)
	}
	group.flush()
	entries := group.entries
	if len(entries) != 2 || entries[0].Process != "web" || entries[0].Source != "stdout" || entries[1].Level != "warn" || entries[1].Message != "two" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

// writeLog writes a log that went quiet a minute ago, so its last row counts as finished.
func writeLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	quiet := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, quiet, quiet); err != nil {
		t.Fatal(err)
	}
}

type fakeSealer struct{ paths []string }

func (f *fakeSealer) SealLogs(string) ([]string, error) { return f.paths, nil }

type fakeApps struct{ snapshots []supervisor.Snapshot }

func (f fakeApps) Snapshots() []supervisor.Snapshot { return f.snapshots }

type memorySink struct {
	entries   []logstore.LogEntry
	offsets   map[string]logstore.TailOffset
	removed   []string
	appendErr error
	countries map[string]string
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

func (m *memorySink) RequestCountries(_ string, ids []string, _ time.Time) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		if country, ok := m.countries[id]; ok {
			out[id] = country
		}
	}
	return out, nil
}

func snapshot(dir string) supervisor.Snapshot {
	return supervisor.Snapshot{Name: "demo", Dir: dir, LogRetention: time.Hour, StdoutRetention: time.Hour}
}

func TestRunOnceIngestsSealedStdoutThenDeletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.log.1.sealed")
	writeLog(t, path, "hello\n")
	sink := &memorySink{}
	module := New(&fakeSealer{paths: []string{path}}, fakeApps{[]supervisor.Snapshot{snapshot(t.TempDir())}}, sink, nil, time.Second)
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
	writeLog(t, segment, "hello\n")
	dir := t.TempDir()
	logDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "production.log")
	writeLog(t, path, "one\n")
	sink := &memorySink{appendErr: errors.New("database unavailable")}
	module := New(&fakeSealer{paths: []string{segment}}, fakeApps{[]supervisor.Snapshot{snapshot(dir)}}, sink, nil, time.Second)
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
	writeLog(t, path, "one\n")
	sink := &memorySink{}
	module := New(&fakeSealer{}, fakeApps{[]supervisor.Snapshot{snapshot(dir)}}, sink, nil, time.Second)

	module.runOnce()
	if len(sink.entries) != 1 || sink.entries[0].Message != "one" || sink.entries[0].Source != "file" || sink.entries[0].Process != "production.log" {
		t.Fatalf("unexpected first pass: %+v", sink.entries)
	}
	if sink.offsets[path].Offset != int64(len("one\n")) {
		t.Fatalf("offset not committed: %+v", sink.offsets[path])
	}

	writeLog(t, path, "one\ntwo\n")
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
	writeLog(t, path, "full\npart")
	sink := &memorySink{}
	module := New(&fakeSealer{}, fakeApps{[]supervisor.Snapshot{snapshot(dir)}}, sink, nil, time.Second)

	module.runOnce()
	if len(sink.entries) != 1 || sink.entries[0].Message != "full" {
		t.Fatalf("partial line should wait: %+v", sink.entries)
	}
	writeLog(t, path, "full\npartial\n")
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
	writeLog(t, path, "long enough first line\n")
	sink := &memorySink{}
	module := New(&fakeSealer{}, fakeApps{[]supervisor.Snapshot{snapshot(dir)}}, sink, nil, time.Second)
	module.runOnce()

	writeLog(t, path, "new\n")
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
	module := New(&fakeSealer{}, fakeApps{[]supervisor.Snapshot{snapshot(dir)}}, sink, nil, time.Second)
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

func TestTailHoldsOpenRowWhileFileIsFresh(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "app.log")
	sink := &memorySink{}
	module := New(&fakeSealer{}, fakeApps{[]supervisor.Snapshot{snapshot(dir)}}, sink, nil, time.Second)

	// Just written: the last row may still get more lines.
	if err := os.WriteFile(path, []byte("done\nhead\n  one\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	module.runOnce()
	if len(sink.entries) != 1 || sink.entries[0].Message != "done" {
		t.Fatalf("open row should wait: %+v", sink.entries)
	}
	if sink.offsets[path].Offset != int64(len("done\n")) {
		t.Fatalf("offset should stay at the open row: %+v", sink.offsets[path])
	}

	writeLog(t, path, "done\nhead\n  one\n  two\n")
	module.runOnce()
	if len(sink.entries) != 2 || sink.entries[1].Message != "head\n  one\n  two" {
		t.Fatalf("row should arrive whole, once: %+v", sink.entries)
	}
	module.runOnce()
	if len(sink.entries) != 2 {
		t.Fatalf("nothing new to read: %+v", sink.entries)
	}
}

// diskSealer hands out every sealed segment in dir, like the supervisor does.
type diskSealer struct{ dir string }

func (d diskSealer) SealLogs(string) ([]string, error) {
	return filepath.Glob(filepath.Join(d.dir, "*.sealed"))
}

func TestSealedRowSplitAcrossSegmentsIsOneRow(t *testing.T) {
	logs := t.TempDir()
	first := filepath.Join(logs, "web.log.1000000000000000001.sealed")
	sink := &memorySink{}
	module := New(diskSealer{logs}, fakeApps{[]supervisor.Snapshot{snapshot(t.TempDir())}}, sink, nil, time.Second)

	// The seal landed in the middle of a record of a busy process.
	if err := os.WriteFile(first, []byte("done\nhead\n  one\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	module.runOnce()
	if len(sink.entries) != 1 || sink.entries[0].Message != "done" {
		t.Fatalf("open row should be carried: %+v", sink.entries)
	}
	carried, err := os.ReadFile(first)
	if err != nil || string(carried) != "head\n  one\n" {
		t.Fatalf("carried segment = %q, %v", carried, err)
	}

	second := filepath.Join(logs, "web.log.1000000000000000002.sealed")
	writeLog(t, second, "  two\nnext\n")
	module.runOnce()
	if len(sink.entries) != 3 || sink.entries[1].Message != "head\n  one\n  two" || sink.entries[2].Message != "next" {
		t.Fatalf("row should be joined across segments: %+v", sink.entries)
	}
	if left, _ := filepath.Glob(filepath.Join(logs, "*")); len(left) != 0 {
		t.Fatalf("segments should be removed: %v", left)
	}
}

func TestSealedCarryFlushesWhenProcessGoesQuiet(t *testing.T) {
	logs := t.TempDir()
	path := filepath.Join(logs, "web.log.1000000000000000001.sealed")
	if err := os.WriteFile(path, []byte("head\n  one\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	module := New(diskSealer{logs}, fakeApps{[]supervisor.Snapshot{snapshot(t.TempDir())}}, sink, nil, time.Second)
	module.runOnce()
	if len(sink.entries) != 0 {
		t.Fatalf("fresh row should be carried: %+v", sink.entries)
	}
	quiet := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, quiet, quiet); err != nil {
		t.Fatal(err)
	}
	module.runOnce()
	if len(sink.entries) != 1 || sink.entries[0].Message != "head\n  one" {
		t.Fatalf("carried row should flush: %+v", sink.entries)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("segment should be removed: %v", err)
	}
}

func TestSealedSegmentsAreRetriedAfterFailedCommit(t *testing.T) {
	logs := t.TempDir()
	writeLog(t, filepath.Join(logs, "web.log.1000000000000000001.sealed"), "one\n")
	writeLog(t, filepath.Join(logs, "worker.log.1000000000000000001.sealed"), "job\n")
	sink := &memorySink{appendErr: errors.New("database unavailable")}
	module := New(diskSealer{logs}, fakeApps{[]supervisor.Snapshot{snapshot(t.TempDir())}}, sink, nil, time.Second)
	module.runOnce()

	sink.appendErr = nil
	writeLog(t, filepath.Join(logs, "web.log.1000000000000000002.sealed"), "two\n")
	module.runOnce()
	if len(sink.entries) != 3 || sink.entries[0].Message != "one" || sink.entries[1].Message != "two" || sink.entries[2].Process != "worker" {
		t.Fatalf("leftover segments should be ingested once, in order: %+v", sink.entries)
	}
	module.runOnce()
	if len(sink.entries) != 3 {
		t.Fatalf("segments were ingested twice: %+v", sink.entries)
	}
}
