package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dboss/internal/supervisor"
)

func writeAppLog(t *testing.T, dir, name, content string) string {
	t.Helper()
	logDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, name)
	writeLog(t, path, content)
	return path
}

func appendAppLog(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func exceptionModule(dir string, sink *memorySink) *Module {
	return New(&fakeSealer{}, fakeApps{[]supervisor.Snapshot{snapshot(dir)}}, sink, nil, time.Second)
}

func TestExceptionLogRoutesAndAggregates(t *testing.T) {
	dir := t.TempDir()
	content := strings.Join([]string{
		`{"uid":"e1","message":"boom","dump":"d1","user":"u1","ip":"1.1.1.1","tags":["a"],"ts":"2026-09-26T10:00:30Z"}`,
		`{"uid":"e1","message":"boom","dump":"d2","user":"u2","ip":"1.1.1.1","ts":"2026-09-26T10:00:45Z"}`,
		`not json`,
		`{"uid":"e1","message":"boom","user":"u3","ip":"2.2.2.2","ts":"2026-09-26T10:01:10Z"}`,
		`{"uid":"e1","message":"boom","ts":"2026-09-26T10:01:20Z"}`,
	}, "\n") + "\n"
	path := writeAppLog(t, dir, "app.exceptions.log", content)
	sink := &memorySink{}
	exceptionModule(dir, sink).runOnce()

	if len(sink.exceptions) != 1 {
		t.Fatalf("batches = %+v", sink.exceptions)
	}
	batch := sink.exceptions[0]
	if batch.Path != path || len(batch.Groups) != 1 || len(batch.Warnings) != 1 {
		t.Fatalf("batch = %+v", batch)
	}
	group := batch.Groups[0]
	if group.ExpUID != "e1" || group.Count != 4 || group.Dump != "d1" || len(group.Minutes) != 2 {
		t.Fatalf("group = %+v", group)
	}
	first := group.Minutes[0]
	if first.Count != 2 || first.Message != "boom" || !first.MinuteAt.Equal(time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("first minute = %+v", first)
	}
	if strings.Join(first.Users, ",") != "u1,u2" || strings.Join(first.IPs, ",") != "1.1.1.1" {
		t.Fatalf("first minute values = %+v", first)
	}
	second := group.Minutes[1]
	if second.Count != 2 || strings.Join(second.Users, ",") != "u3" || strings.Join(second.IPs, ",") != "2.2.2.2" {
		t.Fatalf("second minute = %+v", second)
	}
	if batch.Warnings[0].Level != "warn" || !strings.Contains(batch.Warnings[0].Message, "exception rejected:") {
		t.Fatalf("warning = %+v", batch.Warnings[0])
	}
	if len(sink.entries) != 0 {
		t.Fatalf("exceptions leaked into logs: %+v", sink.entries)
	}
	if want := int64(len(content)); sink.offsets[path].Offset != want {
		t.Fatalf("offset = %d, want %d", sink.offsets[path].Offset, want)
	}

	// A second pass reads nothing new.
	exceptionModule(dir, sink).runOnce()
	if len(sink.exceptions) != 1 {
		t.Fatalf("re-read stored another batch: %+v", sink.exceptions)
	}
}

func TestExceptionHoldsBackPartialLine(t *testing.T) {
	dir := t.TempDir()
	path := writeAppLog(t, dir, "app.exceptions.log", `{"uid":"e","message":"m1","ts":"2026-09-26T10:00:30Z"}`+"\n"+`{"uid":"e","mess`)
	sink := &memorySink{}
	module := exceptionModule(dir, sink)
	module.runOnce()
	if len(sink.exceptions) != 1 || len(sink.exceptions[0].Groups) != 1 || sink.exceptions[0].Groups[0].Count != 1 {
		t.Fatalf("partial line should wait: %+v", sink.exceptions)
	}

	appendAppLog(t, path, `age":"m2","ts":"2026-09-26T10:01:30Z"}`+"\n")
	module.runOnce()
	if len(sink.exceptions) != 2 {
		t.Fatalf("completed line not read: %+v", sink.exceptions)
	}
	group := sink.exceptions[1].Groups[0]
	if group.Count != 1 || len(group.Minutes) != 1 || group.Minutes[0].Message != "m2" {
		t.Fatalf("second batch = %+v", group)
	}
}

func TestExceptionAppendFailureKeepsOffset(t *testing.T) {
	dir := t.TempDir()
	path := writeAppLog(t, dir, "app.exceptions.log", `{"uid":"e","message":"m","ts":"2026-09-26T10:00:30Z"}`+"\n")
	sink := &memorySink{appendErr: errors.New("database unavailable")}
	exceptionModule(dir, sink).runOnce()
	if _, ok := sink.offsets[path]; ok {
		t.Fatal("offset advanced although the batch was not stored")
	}
}

func TestParseExceptionValidates(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	record, err := parseException([]byte(`{"uid":"e","message":"m"}`), now)
	if err != nil || !record.TS.Equal(now) {
		t.Fatalf("missing ts should use ingestion time: %v %+v", err, record)
	}
	if record.Message != "m" {
		t.Fatalf("message = %q", record.Message)
	}
	for name, line := range map[string]string{
		"no uid":      `{"message":"m"}`,
		"exp_uid":     `{"exp_uid":"e","message":"m"}`,
		"no message":  `{"uid":"e"}`,
		"bad message": `{"uid":"e","message":5}`,
		"bad user":    `{"uid":"e","message":"m","user":42}`,
		"bad tags":    `{"uid":"e","message":"m","tags":[1]}`,
		"bad ts":      `{"uid":"e","message":"m","ts":"nope"}`,
		"not json":    `nope`,
	} {
		if _, err := parseException([]byte(line), now); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}
