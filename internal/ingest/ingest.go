// Package ingest turns logs into rows in the log store. It parses dboss's sealed process stdout
// segments, tails the *.log files an app writes under its ./log directory, and copies dboss's own
// daemon log. It is the only place that knows how to parse a log line.
package ingest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"deploy-boss/internal/logstore"
	"deploy-boss/internal/super"
)

// Sealer asks the supervisor to seal one app's process log segments.
type Sealer interface {
	SealLogs(app string) ([]string, error)
}

// Snapshotter lists the apps whose logs should be ingested.
type Snapshotter interface {
	Snapshots() []super.Snapshot
}

// Store is the write side of the log store: it takes parsed rows and remembers how far each app
// log file has been tailed. logstore.Store is the production one.
type Store interface {
	RecordLogs(app string, entries []logstore.LogEntry) error
	TailOffsets(app string) (map[string]logstore.TailOffset, error)
	SaveTailOffset(app, path string, inode uint64, offset int64) error
	RemoveTailOffsets(app string, paths []string) error
}

// Module seals and tails logs on a timer.
type Module struct {
	sealer   Sealer
	apps     Snapshotter
	store    Store
	interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
}

func New(sealer Sealer, apps Snapshotter, store Store, interval time.Duration) *Module {
	return &Module{sealer: sealer, apps: apps, store: store, interval: interval, done: make(chan struct{})}
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

// runOnce ingests every app's sealed stdout segments and tails its app log files. A segment or
// an offset that fails to commit stays for the next pass.
func (m *Module) runOnce() {
	for _, snapshot := range m.apps.Snapshots() {
		if snapshot.LogRetention <= 0 {
			continue
		}
		m.ingestStdout(snapshot)
		m.tailFiles(snapshot)
	}
}

func (m *Module) ingestStdout(snapshot super.Snapshot) {
	if snapshot.StdoutRetention <= 0 {
		return
	}
	sealed, err := m.sealer.SealLogs(snapshot.Name)
	if err != nil {
		log.Printf("seal logs %s: %v", snapshot.Name, err)
		return
	}
	for _, path := range sealed {
		if err := m.ingestSealed(snapshot.Name, path); err != nil {
			log.Printf("ingest %s: %v", path, err)
		}
	}
}

func (m *Module) ingestSealed(app, path string) error {
	entries, err := ParseFile(path, "stdout")
	if err != nil {
		return err
	}
	if err := m.store.RecordLogs(app, entries); err != nil {
		return err
	}
	return os.Remove(path)
}

// tailFiles reads new bytes from every *.log file under the app's ./log directory. The files are
// the app's, not dboss's: they are never rotated or deleted here.
func (m *Module) tailFiles(snapshot super.Snapshot) {
	dir := filepath.Join(snapshot.Dir, "log")
	files := logFiles(dir)
	tracked, err := m.store.TailOffsets(snapshot.Name)
	if err != nil {
		log.Printf("tail offsets %s: %v", snapshot.Name, err)
		return
	}
	var stale []string
	for path := range tracked {
		if !files[path] {
			stale = append(stale, path)
		}
	}
	if err := m.store.RemoveTailOffsets(snapshot.Name, stale); err != nil {
		log.Printf("drop stale offsets %s: %v", snapshot.Name, err)
	}
	for path := range files {
		if err := m.tailFile(snapshot, dir, path, tracked[path]); err != nil {
			log.Printf("tail %s: %v", path, err)
		}
	}
}

func (m *Module) tailFile(snapshot super.Snapshot, dir, path string, previous logstore.TailOffset) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	inode := inodeOf(info)
	start := previous.Offset
	// A replaced or truncated file starts over; the read cursor never survives a new inode.
	if previous.Path == "" || previous.Inode != inode || info.Size() < start {
		start = 0
	}
	if start >= info.Size() {
		if previous.Path == "" || previous.Inode != inode {
			return m.store.SaveTailOffset(snapshot.Name, path, inode, start)
		}
		return nil
	}
	name, err := filepath.Rel(dir, path)
	if err != nil {
		name = filepath.Base(path)
	}
	entries, next, err := parseRange(file, start, "file", name)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		if err := m.store.RecordLogs(snapshot.Name, entries); err != nil {
			return err
		}
	}
	return m.store.SaveTailOffset(snapshot.Name, path, inode, next)
}

// logFiles lists the *.log files under dir, recursively. A missing directory is not an error.
func logFiles(dir string) map[string]bool {
	files := map[string]bool{}
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".log") {
			files[path] = true
		}
		return nil
	})
	return files
}

func inodeOf(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Ino)
	}
	return 0
}

// parseRange reads whole lines starting at offset, returning the entries and the offset just past
// the last complete line. A trailing partial line is left for the next pass.
func parseRange(file *os.File, start int64, source, name string) ([]logstore.LogEntry, int64, error) {
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, start, err
	}
	reader := bufio.NewReaderSize(file, 64*1024)
	var entries []logstore.LogEntry
	next := start
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			entries = append(entries, ParseLine(source, name, strings.TrimRight(string(line), "\r\n")))
			next += int64(len(line))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return entries, next, nil
			}
			return entries, next, err
		}
	}
}

// ParseFile reads a sealed stdout segment into log rows. The process name comes from the file
// name, which is <log_dir>/<app>/<process>.log.<unixnano>.sealed.
func ParseFile(path, source string) ([]logstore.LogEntry, error) {
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
		entries = append(entries, ParseLine(source, name, scanner.Text()))
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
func ParseLine(source, name, line string) logstore.LogEntry {
	entry := logstore.LogEntry{Time: time.Now(), Source: source, Process: name, Stream: "combined", Message: line, Raw: line}
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

// DaemonSink mirrors dboss's own log lines into the reserved host database. It is an io.Writer so
// the daemon can add it to the stdlib logger output next to stderr.
type DaemonSink struct {
	store   Store
	mu      sync.Mutex
	partial []byte
}

func NewDaemonSink(store Store) *DaemonSink { return &DaemonSink{store: store} }

func (s *DaemonSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partial = append(s.partial, p...)
	for {
		index := bytes.IndexByte(s.partial, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimRight(string(s.partial[:index]), "\r")
		s.partial = s.partial[index+1:]
		entry := ParseLine("dboss", "", line)
		_ = s.store.RecordLogs(logstore.HostApp, []logstore.LogEntry{entry})
	}
	return len(p), nil
}

var _ io.Writer = (*DaemonSink)(nil)
