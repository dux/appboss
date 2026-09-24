// Package events is the analytics side of the log store. An app writes one JSON event per line to
// <app dir>/log/<ns>.json.log; ingest parses each line against the contract here and this package
// stores the rows as Parquet under dir/log/<app>/events, partitioned by namespace and UTC day, so
// DuckDB (or any Parquet reader) can query them without a database server and without a lock.
package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Suffix marks an app log file as an event namespace instead of a plain log.
const Suffix = ".json.log"

// Contract limits. A longer msg or tag is cut, not rejected: the event still counts.
const (
	MaxMsg      = 255
	MaxTag      = 128
	MaxTags     = 32
	MaxEvent    = 128
	MaxID       = 256
	SchemaValue = "1"
	SchemaKey   = "dboss.events.schema"
)

// Row is one event as stored in Parquet. An optional string column is null when empty; value is a
// pointer because 0 is a real amount. EID and Country are filled by ingest, not by the app.
type Row struct {
	EID       uint64    `parquet:"eid" json:"eid,string"`
	TS        time.Time `parquet:"ts,timestamp(millisecond:utc)" json:"ts"`
	Event     string    `parquet:"event,dict" json:"event"`
	Msg       string    `parquet:"msg,optional" json:"msg,omitempty"`
	Tags      []string  `parquet:"tags,list" parquet-element:",dict" json:"tags,omitempty"`
	UserID    string    `parquet:"user_id,optional" json:"user_id,omitempty"`
	AnonID    string    `parquet:"anon_id,optional" json:"anon_id,omitempty"`
	TenantID  string    `parquet:"tenant_id,optional,dict" json:"tenant_id,omitempty"`
	RequestID string    `parquet:"request_id,optional" json:"request_id,omitempty"`
	Value     *float64  `parquet:"value,optional" json:"value,omitempty"`
	Country   string    `parquet:"country,optional,dict" json:"country,omitempty"`
	// Data is plain optional text, not the JSON logical type: parquet-go v0.32 panics writing an
	// optional JSON column. The DuckDB views cast it with data::JSON.
	Data string `parquet:"data,optional" json:"data,omitempty"`
}

// fixedKeys are pulled out of data (or the top level) into their own columns.
var fixedKeys = []string{"event", "ts", "user_id", "anon_id", "tenant_id", "request_id", "value"}

// Parse reads one line against the contract:
//
//	{"msg": "...", "tags": ["beta", "plan:pro"], "data": {"event": "checkout", "ts": "...", ...}}
//
// The fixed keys are read from data or from the top level (data wins) and removed from data; any
// other top-level key is folded into data. event is required; ts falls back to now.
func Parse(line []byte, now time.Time) (Row, error) {
	line = bytes.TrimSpace(line)
	var top map[string]json.RawMessage
	if err := unmarshal(line, &top); err != nil || top == nil {
		return Row{}, errors.New("not a JSON object")
	}
	data := map[string]json.RawMessage{}
	if raw, ok := top["data"]; ok && !isNull(raw) {
		if err := unmarshal(raw, &data); err != nil || data == nil {
			return Row{}, errors.New("data is not an object")
		}
	}
	for key, raw := range top {
		switch key {
		case "data", "msg", "tags":
			continue
		}
		if _, ok := data[key]; !ok {
			data[key] = raw
		}
	}

	row := Row{}
	var err error
	if row.Event, err = stringField(data, "event", MaxEvent); err != nil {
		return Row{}, err
	}
	if row.Event == "" {
		return Row{}, errors.New("event is required")
	}
	if row.TS, err = timeField(data["ts"], now); err != nil {
		return Row{}, err
	}
	for key, target := range map[string]*string{"user_id": &row.UserID, "anon_id": &row.AnonID, "tenant_id": &row.TenantID, "request_id": &row.RequestID} {
		if *target, err = stringField(data, key, MaxID); err != nil {
			return Row{}, err
		}
	}
	if raw, ok := data["value"]; ok && !isNull(raw) {
		var value float64
		if unmarshal(raw, &value) != nil {
			return Row{}, errors.New("value is not a number")
		}
		row.Value = &value
	}
	if raw, ok := top["msg"]; ok && !isNull(raw) {
		var msg string
		if unmarshal(raw, &msg) != nil {
			return Row{}, errors.New("msg is not a string")
		}
		row.Msg = cut(strings.TrimSpace(msg), MaxMsg)
	}
	if raw, ok := top["tags"]; ok && !isNull(raw) {
		var tags []string
		if unmarshal(raw, &tags) != nil {
			return Row{}, errors.New("tags is not a list of strings")
		}
		row.Tags = NormalizeTags(tags)
	}
	for _, key := range fixedKeys {
		delete(data, key)
	}
	if len(data) > 0 {
		encoded, err := json.Marshal(data)
		if err != nil {
			return Row{}, fmt.Errorf("data: %w", err)
		}
		row.Data = string(encoded)
	}
	return row, nil
}

// NormalizeTags trims, cuts, dedupes and sorts tags and keeps at most MaxTags of them. A tag with a
// colon is a key:value pair; the key is trimmed on its own so "plan : pro" and "plan:pro" match.
func NormalizeTags(tags []string) []string {
	seen := map[string]bool{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if key, value, ok := strings.Cut(tag, ":"); ok {
			tag = strings.TrimSpace(key) + ":" + strings.TrimSpace(value)
		}
		if tag == "" || tag == ":" {
			continue
		}
		seen[cut(tag, MaxTag)] = true
	}
	out := slices.Sorted(maps.Keys(seen))
	if len(out) > MaxTags {
		out = out[:MaxTags]
	}
	return out
}

// SplitTag returns the key and value of a key:value tag, or "#" and the tag for a plain label.
// The facet index and the filter both use it, so "#" is never a real key.
func SplitTag(tag string) (string, string) {
	if key, value, ok := strings.Cut(tag, ":"); ok {
		return key, value
	}
	return "#", tag
}

var namespacePattern = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*$`)

// Namespace turns a log path relative to the app's ./log (billing/invoice.json.log) into its
// namespace (billing.invoice).
func Namespace(rel string) (string, error) {
	name, ok := strings.CutSuffix(rel, Suffix)
	if !ok {
		return "", fmt.Errorf("%s does not end in %s", rel, Suffix)
	}
	name = strings.ReplaceAll(name, "/", ".")
	if !namespacePattern.MatchString(name) {
		return "", fmt.Errorf("namespace %q: use lowercase letters, digits, _ and -", name)
	}
	return name, nil
}

// IsEventLog reports whether an app log file name is an event namespace.
func IsEventLog(name string) bool { return strings.HasSuffix(name, Suffix) }

func stringField(data map[string]json.RawMessage, key string, limit int) (string, error) {
	raw, ok := data[key]
	if !ok || isNull(raw) {
		return "", nil
	}
	var text string
	if unmarshal(raw, &text) == nil {
		return cut(strings.TrimSpace(text), limit), nil
	}
	// A numeric id is common (user_id: 42) and keeps its digits exactly.
	var number json.Number
	if unmarshal(raw, &number) == nil {
		return number.String(), nil
	}
	return "", fmt.Errorf("%s is not a string", key)
}

// timeField reads ts as an RFC3339 string or a unix millisecond number.
func timeField(raw json.RawMessage, now time.Time) (time.Time, error) {
	if len(raw) == 0 || isNull(raw) {
		return now.UTC().Truncate(time.Millisecond), nil
	}
	var text string
	if unmarshal(raw, &text) == nil {
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return time.Time{}, errors.New("ts is not RFC3339")
		}
		return parsed.UTC().Truncate(time.Millisecond), nil
	}
	var number json.Number
	if unmarshal(raw, &number) == nil {
		ms, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil {
			return time.Time{}, errors.New("ts is not unix milliseconds")
		}
		return time.UnixMilli(ms).UTC(), nil
	}
	return time.Time{}, errors.New("ts is not a string or a number")
}

// unmarshal keeps numbers as json.Number so data survives with its digits intact.
func unmarshal(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data")
	}
	return nil
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// cut shortens s to at most limit runes without splitting one.
func cut(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit])
}
