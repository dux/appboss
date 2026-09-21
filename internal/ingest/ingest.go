// Package ingest turns logs into rows in the log store. It parses dboss's sealed process stdout
// segments, tails the *.log files an app writes under its ./log directory, and copies dboss's own
// daemon log. It is the only place that knows how to parse a log line.
package ingest

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"dboss/internal/logstore"
	"dboss/internal/logx"
	"dboss/internal/super"
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
	AppendLogs(app string, entries []logstore.LogEntry) error
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
		logx.Warnf("seal logs %s: %v", snapshot.Name, err)
		return
	}
	for _, paths := range byProcess(sealed) {
		if err := m.ingestSealed(snapshot.Name, paths); err != nil {
			logx.Warnf("ingest %s: %v", paths[len(paths)-1], err)
		}
	}
}

// byProcess splits sealed segment paths into one list per process log, oldest segment first.
func byProcess(sealed []string) [][]string {
	var keys []string
	groups := map[string][]string{}
	for _, path := range sealed {
		key := filepath.Join(filepath.Dir(path), processName(path))
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], path)
	}
	result := make([][]string, 0, len(keys))
	for _, key := range keys {
		slices.Sort(groups[key])
		result = append(result, groups[key])
	}
	return result
}

// ingestSealed commits one process's segments as a single stream and removes them. A row still
// open at the end of a segment that was written to moments ago is carried into the next pass, so
// a seal never cuts a record in two.
func (m *Module) ingestSealed(app string, paths []string) error {
	newest := paths[len(paths)-1]
	info, err := os.Stat(newest)
	if err != nil {
		return err
	}
	group, err := parseFiles(paths, "stdout")
	if err != nil {
		return err
	}
	hold := group.open() && time.Since(info.ModTime()) < tailQuiet
	if !hold {
		group.flush()
	}
	if err := m.store.AppendLogs(app, group.entries); err != nil {
		return err
	}
	if hold {
		if err := carryTail(newest, group.tail(), info.ModTime()); err != nil {
			return err
		}
		paths = paths[:len(paths)-1]
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// carryTail replaces a sealed segment with just the lines of its open row. The modification time
// is kept, so the carried row is flushed once the process has gone quiet.
func carryTail(path string, lines []string, modified time.Time) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(strings.Join(lines, "\n")+"\n"), 0o640); err != nil {
		return err
	}
	if err := os.Chtimes(temporary, modified, modified); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// tailFiles reads new bytes from every *.log file under the app's ./log directory. The files are
// the app's, not dboss's: they are never rotated or deleted here.
func (m *Module) tailFiles(snapshot super.Snapshot) {
	dir := filepath.Join(snapshot.Dir, "log")
	files, err := logFiles(dir)
	if err != nil {
		// Without a complete listing, a transient error would look like deleted files and the
		// offsets would be dropped, re-reading whole files on the next pass. Skip this round.
		logx.Warnf("list app log files %s: %v", dir, err)
		return
	}
	tracked, err := m.store.TailOffsets(snapshot.Name)
	if err != nil {
		logx.Warnf("tail offsets %s: %v", snapshot.Name, err)
		return
	}
	var stale []string
	for path := range tracked {
		if !files[path] {
			stale = append(stale, path)
		}
	}
	if err := m.store.RemoveTailOffsets(snapshot.Name, stale); err != nil {
		logx.Warnf("drop stale offsets %s: %v", snapshot.Name, err)
	}
	for path := range files {
		if err := m.tailFile(snapshot, dir, path, tracked[path]); err != nil {
			logx.Warnf("tail %s: %v", path, err)
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
	entries, next, err := parseRange(file, start, "file", name, time.Since(info.ModTime()) < tailQuiet)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		if err := m.store.AppendLogs(snapshot.Name, entries); err != nil {
			return err
		}
	}
	return m.store.SaveTailOffset(snapshot.Name, path, inode, next)
}

// logFiles lists the *.log files under dir, recursively. A missing directory is empty, not an
// error; any other walk error is returned so the caller does not mistake it for deleted files.
func logFiles(dir string) (map[string]bool, error) {
	files := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".log") {
			files[path] = true
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return files, nil
}

func inodeOf(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Ino)
	}
	return 0
}

// tailQuiet is how long a log must have been silent before its last row counts as finished.
const tailQuiet = 2 * time.Second

// parseRange reads whole lines starting at offset, returning the entries and the offset to resume
// from. A trailing partial line is left for the next pass, and with hold so is a row that is
// still open, so a pass never cuts a record that is being written.
func parseRange(file *os.File, start int64, source, name string, hold bool) ([]logstore.LogEntry, int64, error) {
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, start, err
	}
	reader := bufio.NewReaderSize(file, 64*1024)
	group := &grouper{source: source, name: name}
	next, rowStart := start, start
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			if group.add(strings.TrimRight(string(line), "\r\n")) {
				rowStart = next
			}
			next += int64(len(line))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, start, err
			}
			if hold && group.open() {
				return group.entries, rowStart, nil
			}
			group.flush()
			return group.entries, next, nil
		}
	}
}

// parseFiles reads one process's sealed stdout segments, oldest first, as a single stream. The
// last row is left open for the caller to flush or carry. The process name comes from the file
// name, which is <log_dir>/<app>/<process>.log.<unixnano>.sealed.
func parseFiles(paths []string, source string) (*grouper, error) {
	group := &grouper{source: source, name: processName(paths[0])}
	for _, path := range paths {
		if err := group.addFile(path); err != nil {
			return nil, err
		}
	}
	return group, nil
}

func (g *grouper) addFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		g.add(scanner.Text())
	}
	return scanner.Err()
}

func processName(path string) string {
	base := path[strings.LastIndexByte(path, '/')+1:]
	if index := strings.Index(base, ".log"); index >= 0 {
		return base[:index]
	}
	return base
}

// ParseLine turns one line into a row. Colors and a leading request id tag are taken out of the
// message; raw keeps the line as written. A JSON object is read for level, message and
// request_id; anything else keeps the whole line as the message and guesses the level from a
// keyword.
func ParseLine(source, name, line string) logstore.LogEntry {
	id, text := splitTag(stripANSI(line))
	entry := logstore.LogEntry{Time: time.Now(), Source: source, Process: name, Stream: "combined", Message: text, RequestID: id, Raw: line}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		var fields jsonLog
		if json.Unmarshal([]byte(trimmed), &fields) == nil {
			entry.Level = strings.ToLower(fields.Level)
			entry.Message = cmp.Or(fields.Message, fields.Msg)
			// Case is kept: the id has to match the one the proxy recorded for the request.
			if fields.RequestID != "" {
				entry.RequestID = fields.RequestID
			}
		}
	}
	if entry.Message == "" {
		entry.Message = text
	}
	if entry.Level == "" {
		entry.Level = detectLevel(text)
	}
	return entry
}

// jsonLog is the part of a structured line dboss reads. Decoding into it rather than a
// map[string]any keeps a JSON line to a couple of allocations; the rest of the object is skipped
// in place instead of being materialized and thrown away.
type jsonLog struct {
	Level     string `json:"level"`
	Message   string `json:"message"`
	Msg       string `json:"msg"`
	RequestID string `json:"request_id"`
}

// levelNames and levelWords are the keywords detectLevel looks for, in priority order. The byte
// copies let it use bytes.Contains, whose search is assembly; returning from levelNames keeps the
// hit allocation-free.
var (
	levelNames = []string{"fatal", "error", "warn", "debug", "info"}
	levelWords = [][]byte{[]byte("fatal"), []byte("error"), []byte("warn"), []byte("debug"), []byte("info")}
)

// detectLevel guesses a level from a keyword. It lowercases into a stack buffer rather than
// strings.ToUpper, which allocated a second copy of every plain line before the five scans.
func detectLevel(line string) string {
	var stack [256]byte
	var lower []byte
	if len(line) <= len(stack) {
		lower = stack[:len(line)]
	} else {
		lower = make([]byte, len(line))
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower[i] = c
	}
	for i, word := range levelWords {
		if bytes.Contains(lower, word) {
			return levelNames[i]
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
