package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dboss/internal/logstore"
	"dboss/internal/supervisor"
)

// ExceptionSuffix marks an app log file as an exception stream instead of a plain log.
const ExceptionSuffix = ".exceptions.log"

// exceptionBatch bounds the lines one pass reads from an exception file.
const exceptionBatch = 50000

// IsExceptionLog reports whether an app log file name is an exception stream.
func IsExceptionLog(name string) bool { return strings.HasSuffix(name, ExceptionSuffix) }

// exceptionLine is one JSON record as the Lux ExceptionWriter emits it. The optional strings are
// pointers so a number or object where a string belongs is rejected rather than coerced.
type exceptionLine struct {
	ExpUID      string   `json:"exp_uid"`
	Message     *string  `json:"message"`
	Dump        *string  `json:"dump"`
	User        *string  `json:"user"`
	IP          *string  `json:"ip"`
	Tags        []string `json:"tags"`
	Description *string  `json:"description"`
	TS          *string  `json:"ts"`
}

// exceptionRecord is a parsed occurrence: the fingerprint plus the fields that fold into a
// minute row.
type exceptionRecord struct {
	ExpUID      string
	TS          time.Time
	Dump        string
	Message     string
	Tags        []string
	Description string
	User        string
	IP          string
}

// parseException reads one line. exp_uid must be a nonempty string and message a string; the
// other optional fields are validated by type and ts falls back to the ingestion time.
func parseException(line []byte, now time.Time) (exceptionRecord, error) {
	var fields exceptionLine
	if err := json.Unmarshal(bytes.TrimSpace(line), &fields); err != nil {
		return exceptionRecord{}, err
	}
	if fields.ExpUID == "" {
		return exceptionRecord{}, errors.New("exp_uid is required")
	}
	if fields.Message == nil {
		return exceptionRecord{}, errors.New("message is required")
	}
	ts := now.UTC()
	if fields.TS != nil {
		parsed, err := time.Parse(time.RFC3339Nano, *fields.TS)
		if err != nil {
			return exceptionRecord{}, errors.New("ts is not RFC3339")
		}
		ts = parsed.UTC()
	}
	return exceptionRecord{
		ExpUID:      fields.ExpUID,
		TS:          ts,
		Dump:        stringValue(fields.Dump),
		Message:     *fields.Message,
		Tags:        fields.Tags,
		Description: stringValue(fields.Description),
		User:        stringValue(fields.User),
		IP:          stringValue(fields.IP),
	}, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// exceptionAggregator folds parsed occurrences into groups keyed by exp_uid and, inside each
// group, one minute row per UTC minute. Order is preserved so the first occurrence owns the
// metadata and the first nonempty dump wins.
type exceptionAggregator struct {
	order  []string
	groups map[string]*logstore.ExceptionGroup
}

func newExceptionAggregator() *exceptionAggregator {
	return &exceptionAggregator{groups: map[string]*logstore.ExceptionGroup{}}
}

func (a *exceptionAggregator) add(record exceptionRecord) {
	group := a.groups[record.ExpUID]
	if group == nil {
		group = &logstore.ExceptionGroup{ExpUID: record.ExpUID}
		a.groups[record.ExpUID] = group
		a.order = append(a.order, record.ExpUID)
	}
	if group.Dump == "" && record.Dump != "" {
		group.Dump = record.Dump
	}
	if group.Count == 0 || record.TS.Before(group.FirstAt) {
		group.FirstAt = record.TS
	}
	if record.TS.After(group.LastAt) {
		group.LastAt = record.TS
	}
	group.Count++

	minuteAt := record.TS.Truncate(time.Minute)
	index := -1
	for i := range group.Minutes {
		if group.Minutes[i].MinuteAt.Equal(minuteAt) {
			index = i
			break
		}
	}
	if index < 0 {
		group.Minutes = append(group.Minutes, logstore.ExceptionMinute{
			MinuteAt:    minuteAt,
			Message:     record.Message,
			Tags:        record.Tags,
			Description: record.Description,
		})
		index = len(group.Minutes) - 1
	}
	group.Minutes[index].Count++
	group.Minutes[index].Users = logstore.AppendUniqueValues(group.Minutes[index].Users, record.User)
	group.Minutes[index].IPs = logstore.AppendUniqueValues(group.Minutes[index].IPs, record.IP)
}

func (a *exceptionAggregator) result() []logstore.ExceptionGroup {
	groups := make([]logstore.ExceptionGroup, 0, len(a.order))
	for _, uid := range a.order {
		groups = append(groups, *a.groups[uid])
	}
	return groups
}

// tailExceptions reads new whole lines from an exception file, aggregates them and commits the
// groups, malformed-line warnings and the new offset in one transaction. A trailing partial line
// waits for the rest of it, and the offset never advances without that commit.
func (m *Module) tailExceptions(snapshot supervisor.Snapshot, dir, path string, previous logstore.TailOffset) error {
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
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(file, 64*1024)
	now := time.Now()
	aggregator := newExceptionAggregator()
	var warnings []logstore.LogEntry
	next := start
	for lines := 0; lines < exceptionBatch; lines++ {
		line, err := reader.ReadBytes('\n')
		if len(line) == 0 || line[len(line)-1] != '\n' {
			break
		}
		next += int64(len(line))
		if len(bytes.TrimSpace(line)) > 0 {
			record, parseErr := parseException(line, now)
			if parseErr != nil {
				text := strings.TrimRight(string(line), "\r\n")
				warnings = append(warnings, logstore.LogEntry{Time: now, Source: "file", Process: name, Stream: "combined", Level: "warn", Message: "exception rejected: " + parseErr.Error(), Raw: text})
			} else {
				aggregator.add(record)
			}
		}
		if err != nil {
			break
		}
	}
	return m.store.AppendExceptions(snapshot.Name, logstore.ExceptionBatch{
		Path:     path,
		Inode:    inode,
		Offset:   next,
		Groups:   aggregator.result(),
		Warnings: warnings,
	})
}
