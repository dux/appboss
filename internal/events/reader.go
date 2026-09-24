package events

import (
	"cmp"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultRange is the window a summary covers when the filter names none.
const DefaultRange = 30 * 24 * time.Hour

// MaxLatest bounds how many events one Latest call returns.
const MaxLatest = 500

// DayCount is one event's count on one day, the unit of the summary chart.
type DayCount struct {
	Date     string  `json:"date"`
	NS       string  `json:"ns"`
	Event    string  `json:"event"`
	Count    int64   `json:"count"`
	Users    int64   `json:"users"`
	Anons    int64   `json:"anons"`
	ValueSum float64 `json:"value_sum"`
}

// Summary is an app's events over a filter's range.
type Summary struct {
	From       string     `json:"from"`
	To         string     `json:"to"`
	Namespaces []string   `json:"namespaces"`
	Days       []DayCount `json:"days"`
	Count      int64      `json:"count"`
	ValueSum   float64    `json:"value_sum"`
	// Users and Anons are distinct over the whole range when Scanned; from the indexes they are
	// the largest single day, since daily distinct counts do not add up.
	Users   int64 `json:"users"`
	Anons   int64 `json:"anons"`
	Scanned bool  `json:"scanned"`
	// RawFrom is the oldest day raw events still exist for; older days only have counts.
	RawFrom string `json:"raw_from,omitempty"`
	Bytes   int64  `json:"bytes"`
}

// Reader answers the console and CLI from the files alone, without DuckDB: counts and facets from
// the indexes when the filter allows it, otherwise by scanning the raw days in range.
type Reader struct {
	store *Store
	mu    sync.Mutex
	open  map[string]cachedIndex // index of a day that is not compacted yet, by partition dir
}

type cachedIndex struct {
	signature string
	index     Index
}

func NewReader(store *Store) *Reader { return &Reader{store: store, open: map[string]cachedIndex{}} }

// window turns a filter into the dates it covers. A filter without a range covers DefaultRange.
func window(filter Filter, now time.Time) (time.Time, time.Time, string, string) {
	from, to := filter.Range(now)
	if from.IsZero() {
		from = now.Add(-DefaultRange)
	}
	if to.IsZero() {
		to = now
	}
	return from, to, from.UTC().Format(DateLayout), to.UTC().Format(DateLayout)
}

// Summary counts an app's events per day and event over the filter's range.
func (r *Reader) Summary(app string, filter Filter, now time.Time) (Summary, error) {
	from, to, fromDate, toDate := window(filter, now)
	summary := Summary{From: fromDate, To: toDate, Bytes: r.store.Bytes(app)}
	partitions, err := r.store.Partitions(app)
	if err != nil {
		return summary, err
	}
	namespaces := map[string]bool{}
	for _, partition := range partitions {
		namespaces[partition.NS] = true
		if summary.RawFrom == "" || partition.Date < summary.RawFrom {
			summary.RawFrom = partition.Date
		}
	}
	// Namespaces whose raw days are gone still have indexes.
	dates, err := r.store.IndexDates(app)
	if err != nil {
		return summary, err
	}

	if filter.IndexOnly() {
		days := map[string]bool{}
		for _, partition := range partitions {
			if partition.Date < fromDate || partition.Date > toDate || !nsAllowed(filter, partition.NS) {
				continue
			}
			index, err := r.index(app, partition)
			if err != nil {
				return summary, err
			}
			days[partition.Date+"/"+partition.NS] = true
			summary.add(partition.Date, filter, index.Daily)
		}
		for _, date := range dates {
			if date < fromDate || date > toDate {
				continue
			}
			for _, ns := range r.store.indexNamespaces(app, date) {
				namespaces[ns] = true
				if days[date+"/"+ns] || !nsAllowed(filter, ns) {
					continue
				}
				index, ok, err := r.store.readIndex(app, ns, date)
				if err != nil {
					return summary, err
				}
				if ok {
					summary.add(date, filter, index.Daily)
				}
			}
		}
	} else {
		summary.Scanned = true
		users, anons := map[string]bool{}, map[string]bool{}
		days := map[[3]string]*DayCount{}
		dayUsers := map[[3]string]map[string]bool{}
		dayAnons := map[[3]string]map[string]bool{}
		err := r.scan(app, partitions, fromDate, toDate, func(ns, date string, row Row) bool {
			if !filter.Match(row, ns, now) || row.TS.Before(from) || row.TS.After(to) {
				return true
			}
			key := [3]string{date, ns, row.Event}
			day := days[key]
			if day == nil {
				day = &DayCount{Date: date, NS: ns, Event: row.Event}
				days[key] = day
				dayUsers[key], dayAnons[key] = map[string]bool{}, map[string]bool{}
			}
			day.Count++
			if row.Value != nil {
				day.ValueSum += *row.Value
			}
			if row.UserID != "" {
				dayUsers[key][row.UserID] = true
				users[row.UserID] = true
			}
			if row.AnonID != "" {
				dayAnons[key][row.AnonID] = true
				anons[row.AnonID] = true
			}
			return true
		})
		if err != nil {
			return summary, err
		}
		for key, day := range days {
			day.Users, day.Anons = int64(len(dayUsers[key])), int64(len(dayAnons[key]))
			summary.Days = append(summary.Days, *day)
			summary.Count += day.Count
			summary.ValueSum += day.ValueSum
		}
		summary.Users, summary.Anons = int64(len(users)), int64(len(anons))
	}
	summary.Namespaces = slices.Sorted(maps.Keys(namespaces))
	slices.SortFunc(summary.Days, func(a, b DayCount) int {
		return cmp.Or(strings.Compare(a.Date, b.Date), strings.Compare(a.NS, b.NS), strings.Compare(a.Event, b.Event))
	})
	return summary, nil
}

func (s *Summary) add(date string, filter Filter, rows []DailyRow) {
	for _, row := range rows {
		if !eventAllowed(filter, row.Event) {
			continue
		}
		s.Days = append(s.Days, DayCount{Date: date, NS: row.NS, Event: row.Event, Count: row.Count, Users: row.Users, Anons: row.Anons, ValueSum: row.ValueSum})
		s.Count += row.Count
		s.ValueSum += row.ValueSum
		s.Users = max(s.Users, row.Users)
		s.Anons = max(s.Anons, row.Anons)
	}
}

// nsAllowed and eventAllowed apply the ns and the event terms of an index-only filter to index
// rows, which carry no other columns.
func nsAllowed(filter Filter, ns string) bool {
	return filter.only(func(t Term) bool { return t.Kind == FieldTerm && t.Field == "ns" }).Match(Row{}, ns, time.Time{})
}

func eventAllowed(filter Filter, event string) bool {
	return filter.only(func(t Term) bool { return t.Kind == EventTerm || t.Kind == EventPrefixTerm }).Match(Row{Event: event}, "", time.Time{})
}

// index is a partition's stored index when the day is compacted, else one built from its raw
// files and cached until they change.
func (r *Reader) index(app string, partition Partition) (Index, error) {
	if partition.Compact() {
		if index, ok, err := r.store.readIndex(app, partition.NS, partition.Date); err != nil || ok {
			r.mu.Lock()
			delete(r.open, partition.Dir)
			r.mu.Unlock()
			return index, err
		}
	}
	signature := signatureOf(partition.Files)
	r.mu.Lock()
	cached, ok := r.open[partition.Dir]
	r.mu.Unlock()
	if ok && cached.signature == signature {
		return cached.index, nil
	}
	rows, err := readPartition(partition)
	if err != nil {
		return Index{}, err
	}
	index := BuildIndex(partition.NS, Dedupe(rows))
	r.mu.Lock()
	r.open[partition.Dir] = cachedIndex{signature: signature, index: index}
	r.mu.Unlock()
	return index, nil
}

func signatureOf(files []string) string {
	var out strings.Builder
	for _, file := range files {
		out.WriteString(file)
		if info, err := os.Stat(file); err == nil {
			out.WriteString(info.ModTime().String())
		}
		out.WriteByte('\n')
	}
	return out.String()
}

func readPartition(partition Partition) ([]Row, error) {
	var rows []Row
	for _, file := range partition.Files {
		batch, err := readParquet[Row](file)
		if err != nil {
			return nil, err
		}
		rows = append(rows, batch...)
	}
	return rows, nil
}

// scan calls visit for every raw row of the days in [fromDate, toDate], oldest day first, until
// visit returns false.
func (r *Reader) scan(app string, partitions []Partition, fromDate, toDate string, visit func(ns, date string, row Row) bool) error {
	for _, partition := range partitions {
		if partition.Date < fromDate || partition.Date > toDate {
			continue
		}
		rows, err := readPartition(partition)
		if err != nil {
			return err
		}
		for _, row := range Dedupe(rows) {
			if !visit(partition.NS, partition.Date, row) {
				return nil
			}
		}
	}
	return nil
}

// Event is a row with its namespace, as the latest-events list shows it.
type Event struct {
	NS string `json:"ns"`
	Row
}

// Latest returns the newest events matching the filter, newest first.
func (r *Reader) Latest(app string, filter Filter, limit int, now time.Time) ([]Event, error) {
	limit = min(max(limit, 1), MaxLatest)
	_, _, fromDate, toDate := window(filter, now)
	partitions, err := r.store.Partitions(app)
	if err != nil {
		return nil, err
	}
	// Newest day first; within a day every namespace is read before deciding, since their events
	// interleave.
	slices.SortFunc(partitions, func(a, b Partition) int { return strings.Compare(b.Date, a.Date) })
	var out []Event
	for i := 0; i < len(partitions); {
		date := partitions[i].Date
		var day []Event
		for ; i < len(partitions) && partitions[i].Date == date; i++ {
			partition := partitions[i]
			if date < fromDate || date > toDate {
				continue
			}
			rows, err := readPartition(partition)
			if err != nil {
				return nil, err
			}
			for _, row := range Dedupe(rows) {
				if filter.Match(row, partition.NS, now) {
					day = append(day, Event{NS: partition.NS, Row: row})
				}
			}
		}
		slices.SortFunc(day, func(a, b Event) int { return b.TS.Compare(a.TS) })
		out = append(out, day...)
		if len(out) >= limit {
			return out[:limit], nil
		}
	}
	return out, nil
}

// Facet is one value of a tag key, or one data key, with how often it occurs.
type Facet struct {
	Value string `json:"value"`
	Type  string `json:"type,omitempty"` // for data keys: the JSON type
	Count int64  `json:"count"`
}

// MaxFacets bounds a facet answer.
const MaxFacets = 50

// Facets lists the values a tag key takes (key "plan"), the labels (key "#"), the tag keys (key
// "") or the data keys (key "data.") among the events the filter matches, most frequent first.
// An index-only filter is answered from the facet and key indexes.
func (r *Reader) Facets(app string, filter Filter, key string, now time.Time) ([]Facet, error) {
	_, _, fromDate, toDate := window(filter, now)
	partitions, err := r.store.Partitions(app)
	if err != nil {
		return nil, err
	}
	counts := map[[2]string]int64{}
	count := func(value, kind string, n int64) { counts[[2]string{value, kind}] += n }
	collect := func(index Index) {
		if key == "data." {
			for _, row := range index.Keys {
				if eventAllowed(filter, row.Event) {
					count(row.Path, row.Type, row.Count)
				}
			}
			return
		}
		for _, row := range index.Facets {
			if !eventAllowed(filter, row.Event) {
				continue
			}
			switch {
			case key == "" && row.Key != "#":
				count(row.Key, "", row.Count)
			case row.Key == key:
				count(row.Value, "", row.Count)
			}
		}
	}
	if filter.IndexOnly() {
		seen := map[string]bool{}
		for _, partition := range partitions {
			if partition.Date < fromDate || partition.Date > toDate || !nsAllowed(filter, partition.NS) {
				continue
			}
			index, err := r.index(app, partition)
			if err != nil {
				return nil, err
			}
			seen[partition.Date+"/"+partition.NS] = true
			collect(index)
		}
		dates, err := r.store.IndexDates(app)
		if err != nil {
			return nil, err
		}
		for _, date := range dates {
			if date < fromDate || date > toDate {
				continue
			}
			for _, ns := range r.store.indexNamespaces(app, date) {
				if seen[date+"/"+ns] || !nsAllowed(filter, ns) {
					continue
				}
				if index, ok, err := r.store.readIndex(app, ns, date); err != nil {
					return nil, err
				} else if ok {
					collect(index)
				}
			}
		}
	} else {
		byNS := map[string][]Row{}
		err := r.scan(app, partitions, fromDate, toDate, func(ns, _ string, row Row) bool {
			if filter.Match(row, ns, now) {
				byNS[ns] = append(byNS[ns], row)
			}
			return true
		})
		if err != nil {
			return nil, err
		}
		for ns, nsRows := range byNS {
			collect(BuildIndex(ns, nsRows))
		}
	}
	out := make([]Facet, 0, len(counts))
	for k, n := range counts {
		out = append(out, Facet{Value: k[0], Type: k[1], Count: n})
	}
	slices.SortFunc(out, func(a, b Facet) int { return cmp.Or(cmp.Compare(b.Count, a.Count), strings.Compare(a.Value, b.Value)) })
	if len(out) > MaxFacets {
		out = out[:MaxFacets]
	}
	return out, nil
}
