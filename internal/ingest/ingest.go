// Package ingest turns sealed process-log segments into rows in the log store. It is the only
// place that knows how to parse a log line, so a new source just gets its own parser.
package ingest

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"deploy-boss/internal/logstore"
	"deploy-boss/internal/super"
)

// Sealer asks the supervisor to seal one app's process log segments.
type Sealer interface {
	SealLogs(app string) ([]string, error)
}

// Snapshotter lists the apps whose logs should be sealed.
type Snapshotter interface {
	Snapshots() []super.Snapshot
}

// Sink receives parsed entries; logstore.Store is the production one.
type Sink interface {
	RecordLogs(app string, entries []logstore.LogEntry) error
}

// Module seals and ingests process logs on a timer.
type Module struct {
	sealer   Sealer
	apps     Snapshotter
	sink     Sink
	interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
}

func New(sealer Sealer, apps Snapshotter, sink Sink, interval time.Duration) *Module {
	return &Module{sealer: sealer, apps: apps, sink: sink, interval: interval, done: make(chan struct{})}
}

func (m *Module) Name() string { return "ingest" }

func (m *Module) Start(ctx context.Context) error {
	ctx, m.cancel = context.WithCancel(ctx)
	go m.loop(ctx)
	return nil
}

func (m *Module) Close() error {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
	return nil
}

func (m *Module) loop(ctx context.Context) {
	defer close(m.done)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.runOnce()
		}
	}
}

// runOnce seals every app's current segments, ingests them and deletes the ones that made it to
// the store. A segment that fails to enqueue stays on disk for the next pass.
func (m *Module) runOnce() {
	for _, snapshot := range m.apps.Snapshots() {
		sealed, err := m.sealer.SealLogs(snapshot.Name)
		if err != nil {
			log.Printf("seal logs %s: %v", snapshot.Name, err)
			continue
		}
		for _, path := range sealed {
			if err := m.ingestFile(snapshot.Name, path); err != nil {
				log.Printf("ingest %s: %v", path, err)
			}
		}
	}
}

func (m *Module) ingestFile(app, path string) error {
	entries, err := ParseFile(path, app)
	if err != nil {
		return err
	}
	if err := m.sink.RecordLogs(app, entries); err != nil {
		return err
	}
	return os.Remove(path)
}

// ParseFile reads a sealed segment into log rows. The process name comes from the file name,
// which is <log_dir>/<app>/<process>.log.<unixnano>.sealed.
func ParseFile(path, app string) ([]logstore.LogEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	name := processName(path)
	entries := make([]logstore.LogEntry, 0, 256)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		entries = append(entries, ParseLine(name, scanner.Text()))
	}
	return entries, scanner.Err()
}

func processName(path string) string {
	base := path[strings.LastIndexByte(path, '/')+1:]
	if index := strings.Index(base, ".log"); index >= 0 {
		return base[:index]
	}
	return base
}

// ParseLine turns one line into a row. A JSON object is read for level, message and request_id;
// anything else keeps the whole line as the message and guesses the level from a keyword.
func ParseLine(process, line string) logstore.LogEntry {
	entry := logstore.LogEntry{Time: time.Now(), Source: "process", Process: process, Stream: "combined", Message: line, Raw: line}
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "{") {
		var fields map[string]any
		if json.Unmarshal([]byte(trimmed), &fields) == nil {
			entry.Level = stringField(fields, "level")
			entry.Message = firstString(fields, "message", "msg")
			entry.RequestID = stringField(fields, "request_id")
		}
	}
	if entry.Message == "" {
		entry.Message = line
	}
	if entry.Level == "" {
		entry.Level = detectLevel(line)
	}
	return entry
}

func firstString(fields map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := fields[key].(string); ok {
			return value
		}
	}
	return ""
}

func stringField(fields map[string]any, key string) string {
	value, _ := fields[key].(string)
	return strings.ToLower(value)
}

func detectLevel(line string) string {
	upper := strings.ToUpper(line)
	for _, level := range []string{"FATAL", "ERROR", "WARN", "DEBUG", "INFO"} {
		if strings.Contains(upper, level) {
			return strings.ToLower(level)
		}
	}
	return "info"
}
