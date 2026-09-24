package events

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The indexes of one namespace's day. They are small (a row per event, tag value or data key),
// are written by compaction for a closed day and computed from the raw files for an open one, and
// are never pruned by retention: counts and facets outlive the raw events.
const (
	DailyIndex = "_daily"
	FacetIndex = "_facets"
	KeyIndex   = "_keys"
)

// DailyRow counts one event of one namespace on one day. Date comes from the directory.
type DailyRow struct {
	NS       string  `parquet:"ns,dict" json:"ns"`
	Event    string  `parquet:"event,dict" json:"event"`
	Count    int64   `parquet:"count" json:"count"`
	Users    int64   `parquet:"users" json:"users"`
	Anons    int64   `parquet:"anons" json:"anons"`
	ValueSum float64 `parquet:"value_sum" json:"value_sum"`
}

// FacetRow counts one tag value. A key:value tag is Key=key; a plain label is Key="#".
type FacetRow struct {
	NS    string `parquet:"ns,dict" json:"ns"`
	Event string `parquet:"event,dict" json:"event"`
	Key   string `parquet:"key,dict" json:"key"`
	Value string `parquet:"value" json:"value"`
	Count int64  `parquet:"count" json:"count"`
}

// KeyRow counts one top-level data key and its JSON type, for autocompleting data.* filters.
type KeyRow struct {
	NS    string `parquet:"ns,dict" json:"ns"`
	Event string `parquet:"event,dict" json:"event"`
	Path  string `parquet:"path" json:"path"`
	Type  string `parquet:"type,dict" json:"type"`
	Count int64  `parquet:"count" json:"count"`
}

// Index is the three indexes of one namespace's day.
type Index struct {
	Daily  []DailyRow
	Facets []FacetRow
	Keys   []KeyRow
}

// BuildIndex aggregates rows (already deduplicated) of one namespace.
func BuildIndex(ns string, rows []Row) Index {
	type daily struct {
		count        int64
		users, anons map[string]bool
		sum          float64
	}
	type facet struct{ event, key, value string }
	type key struct{ event, path, kind string }
	days := map[string]*daily{}
	facets := map[facet]int64{}
	keys := map[key]int64{}
	for _, row := range rows {
		entry := days[row.Event]
		if entry == nil {
			entry = &daily{users: map[string]bool{}, anons: map[string]bool{}}
			days[row.Event] = entry
		}
		entry.count++
		if row.UserID != "" {
			entry.users[row.UserID] = true
		}
		if row.AnonID != "" {
			entry.anons[row.AnonID] = true
		}
		if row.Value != nil {
			entry.sum += *row.Value
		}
		for _, tag := range row.Tags {
			name, value := SplitTag(tag)
			facets[facet{row.Event, name, value}]++
		}
		if row.Data != "" {
			var data map[string]json.RawMessage
			if json.Unmarshal([]byte(row.Data), &data) == nil {
				for path, raw := range data {
					keys[key{row.Event, path, jsonType(raw)}]++
				}
			}
		}
	}
	var index Index
	for _, event := range slices.Sorted(maps.Keys(days)) {
		entry := days[event]
		index.Daily = append(index.Daily, DailyRow{NS: ns, Event: event, Count: entry.count, Users: int64(len(entry.users)), Anons: int64(len(entry.anons)), ValueSum: entry.sum})
	}
	for f, count := range facets {
		index.Facets = append(index.Facets, FacetRow{NS: ns, Event: f.event, Key: f.key, Value: f.value, Count: count})
	}
	slices.SortFunc(index.Facets, func(a, b FacetRow) int {
		return cmp.Or(strings.Compare(a.Event, b.Event), strings.Compare(a.Key, b.Key), strings.Compare(a.Value, b.Value))
	})
	for k, count := range keys {
		index.Keys = append(index.Keys, KeyRow{NS: ns, Event: k.event, Path: k.path, Type: k.kind, Count: count})
	}
	slices.SortFunc(index.Keys, func(a, b KeyRow) int {
		return cmp.Or(strings.Compare(a.Event, b.Event), strings.Compare(a.Path, b.Path), strings.Compare(a.Type, b.Type))
	})
	return index
}

func jsonType(raw json.RawMessage) string {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '"':
			return "string"
		case '{':
			return "object"
		case '[':
			return "array"
		case 't', 'f':
			return "boolean"
		case 'n':
			return "null"
		default:
			return "number"
		}
	}
	return "null"
}

func (s *Store) indexPath(app, index, ns, date string) string {
	return filepath.Join(s.Dir(app), index, "date="+date, ns+parquetExt)
}

// writeIndex writes the three index files of one namespace's day.
func (s *Store) writeIndex(app, ns, date string, index Index) error {
	for _, dir := range []string{DailyIndex, FacetIndex, KeyIndex} {
		if err := os.MkdirAll(filepath.Dir(s.indexPath(app, dir, ns, date)), 0o755); err != nil {
			return err
		}
	}
	if err := writeParquet(s.indexPath(app, DailyIndex, ns, date), index.Daily); err != nil {
		return err
	}
	if err := writeParquet(s.indexPath(app, FacetIndex, ns, date), index.Facets); err != nil {
		return err
	}
	return writeParquet(s.indexPath(app, KeyIndex, ns, date), index.Keys)
}

// hasIndex reports whether all three index files of a namespace's day exist.
func (s *Store) hasIndex(app, ns, date string) bool {
	for _, dir := range []string{DailyIndex, FacetIndex, KeyIndex} {
		if _, err := os.Stat(s.indexPath(app, dir, ns, date)); err != nil {
			return false
		}
	}
	return true
}

// readIndex reads the stored index of a namespace's day. ok is false when any file is missing.
func (s *Store) readIndex(app, ns, date string) (Index, bool, error) {
	if !s.hasIndex(app, ns, date) {
		return Index{}, false, nil
	}
	var index Index
	var err error
	if index.Daily, err = readParquet[DailyRow](s.indexPath(app, DailyIndex, ns, date)); err != nil {
		return Index{}, false, err
	}
	if index.Facets, err = readParquet[FacetRow](s.indexPath(app, FacetIndex, ns, date)); err != nil {
		return Index{}, false, err
	}
	if index.Keys, err = readParquet[KeyRow](s.indexPath(app, KeyIndex, ns, date)); err != nil {
		return Index{}, false, err
	}
	return index, true, nil
}

// IndexDates lists the dates that have a stored daily index, so summaries reach days whose raw
// events retention already removed.
func (s *Store) IndexDates(app string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir(app), DailyIndex))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if date, ok := strings.CutPrefix(entry.Name(), "date="); ok && entry.IsDir() {
			out = append(out, date)
		}
	}
	slices.Sort(out)
	return out, nil
}

// indexNamespaces lists the namespaces that have a stored index on a date.
func (s *Store) indexNamespaces(app, date string) []string {
	files, _ := parquetFiles(filepath.Join(s.Dir(app), DailyIndex, "date="+date))
	var out []string
	for _, file := range files {
		out = append(out, strings.TrimSuffix(filepath.Base(file), parquetExt))
	}
	return out
}
