package events

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/parquet-go/parquet-go"
)

// On-disk layout under dir/log/<app>/events:
//
//	ns=<ns>/date=<YYYY-MM-DD>/<unixnano>-<seq>.parquet   one ingest batch
//	ns=<ns>/date=<YYYY-MM-DD>/day-<unixnano>.parquet     the compacted day (deduped, sorted)
//	_daily|_facets|_keys/date=<YYYY-MM-DD>/<ns>.parquet  indexes of a compacted day
//	views.sql                                            CREATE VIEW statements for DuckDB
//
// Every file is written to a .tmp name and renamed, so a reader never sees half a file.
const (
	DirName     = "events"
	DateLayout  = "2006-01-02"
	dayPrefix   = "day-"
	parquetExt  = ".parquet"
	tmpExt      = ".tmp"
	journalName = ".compact"
	// rowGroupRows keeps row groups small enough that min/max statistics on the sort key skip
	// most of a day for a tenant or event filter.
	rowGroupRows = 64 * 1024
)

// Store writes and reads the Parquet files of every app. It holds no state besides the root, so
// ingest, the daily job and the readers can each have their own.
type Store struct {
	root string // dir/log
}

func NewStore(logDir string) *Store { return &Store{root: logDir} }

// Dir is the events directory of one app.
func (s *Store) Dir(app string) string { return filepath.Join(s.root, app, DirName) }

func (s *Store) partitionDir(app, ns, date string) string {
	return filepath.Join(s.Dir(app), "ns="+ns, "date="+date)
}

var batchSeq atomic.Uint64

// Append writes one batch per UTC day the rows fall on. It returns only after the files are in
// place, so ingest can advance its offset on success.
func (s *Store) Append(app, ns string, rows []Row) error {
	byDate := map[string][]Row{}
	for _, row := range rows {
		date := row.TS.UTC().Format(DateLayout)
		byDate[date] = append(byDate[date], row)
	}
	for date, batch := range byDate {
		dir := s.partitionDir(app, ns, date)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		name := fmt.Sprintf("%d-%d%s", time.Now().UnixNano(), batchSeq.Add(1), parquetExt)
		if err := writeParquet(filepath.Join(dir, name), batch, eventOptions()...); err != nil {
			return err
		}
	}
	return nil
}

// writeParquet writes rows to path through a .tmp file and a rename.
func writeParquet[T any](path string, rows []T, options ...parquet.WriterOption) error {
	temporary := path + tmpExt
	file, err := os.Create(temporary)
	if err != nil {
		return err
	}
	options = append([]parquet.WriterOption{
		parquet.Compression(&parquet.Zstd),
		parquet.KeyValueMetadata(SchemaKey, SchemaValue),
		parquet.MaxRowsPerRowGroup(rowGroupRows),
	}, options...)
	writer := parquet.NewGenericWriter[T](file, options...)
	_, err = writer.Write(rows)
	if err == nil {
		err = writer.Close()
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(temporary)
		return err
	}
	return os.Rename(temporary, path)
}

// eventOptions are the writer options of an event file: bloom filters on the id columns, so an
// equality lookup on a user or a request can skip row groups.
func eventOptions() []parquet.WriterOption {
	return []parquet.WriterOption{parquet.BloomFilters(
		parquet.SplitBlockFilter(10, "user_id"),
		parquet.SplitBlockFilter(10, "anon_id"),
		parquet.SplitBlockFilter(10, "request_id"),
	)}
}

func readParquet[T any](path string) ([]T, error) {
	rows, err := parquet.ReadFile[T](path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return rows, nil
}

// Partition is one namespace's day on disk.
type Partition struct {
	NS    string
	Date  string
	Dir   string
	Files []string // *.parquet, oldest first
}

// Compact reports whether the day is a single compacted file.
func (p Partition) Compact() bool {
	return len(p.Files) == 1 && strings.HasPrefix(filepath.Base(p.Files[0]), dayPrefix)
}

// Partitions lists every ns/date directory of an app, sorted by date then namespace. A missing
// events directory is empty.
func (s *Store) Partitions(app string) ([]Partition, error) {
	root := s.Dir(app)
	namespaces, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Partition
	for _, nsEntry := range namespaces {
		ns, ok := strings.CutPrefix(nsEntry.Name(), "ns=")
		if !ok || !nsEntry.IsDir() {
			continue
		}
		dates, err := os.ReadDir(filepath.Join(root, nsEntry.Name()))
		if err != nil {
			return nil, err
		}
		for _, dateEntry := range dates {
			date, ok := strings.CutPrefix(dateEntry.Name(), "date=")
			if !ok || !dateEntry.IsDir() {
				continue
			}
			dir := filepath.Join(root, nsEntry.Name(), dateEntry.Name())
			files, err := parquetFiles(dir)
			if err != nil {
				return nil, err
			}
			out = append(out, Partition{NS: ns, Date: date, Dir: dir, Files: files})
		}
	}
	slices.SortFunc(out, func(a, b Partition) int {
		return cmp.Or(strings.Compare(a.Date, b.Date), strings.Compare(a.NS, b.NS))
	})
	return out, nil
}

// parquetFiles lists the finished *.parquet files of a directory, oldest name first. Batch names
// start with a unix nanosecond, so name order is write order; the day file sorts after digits.
func parquetFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), parquetExt) {
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	slices.Sort(files)
	return files, nil
}

// Namespaces lists the namespaces an app has events for.
func (s *Store) Namespaces(app string) ([]string, error) {
	entries, err := os.ReadDir(s.Dir(app))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if ns, ok := strings.CutPrefix(entry.Name(), "ns="); ok && entry.IsDir() {
			out = append(out, ns)
		}
	}
	return out, nil
}

// Apps lists the app directories under the log root that hold an events directory, including
// apps no longer in the config, so retention still reaches their files.
func (s *Store) Apps() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if info, err := os.Stat(filepath.Join(s.root, entry.Name(), DirName)); err == nil && info.IsDir() {
			out = append(out, entry.Name())
		}
	}
	return out, nil
}

// Bytes is the size of an app's events directory.
func (s *Store) Bytes(app string) int64 {
	var total int64
	filepath.WalkDir(s.Dir(app), func(_ string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			if info, err := entry.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// Today is the current UTC day; days before it are closed and can be compacted.
func Today(now time.Time) string { return now.UTC().Format(DateLayout) }
