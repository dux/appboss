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
	"time"

	"dboss/internal/fsutil"
)

// journal is written before a compacted day file replaces its sources, so a crash between the
// rename and the deletes is finished on the next run instead of leaving every row twice.
type journal struct {
	Target  string   `json:"target"`
	Sources []string `json:"sources"`
}

// CompactApp compacts every closed day of an app that is not already one day file with indexes.
// Days are independent: one failing is logged by the caller and does not stop the others.
func (s *Store) CompactApp(app string, now time.Time) error {
	partitions, err := s.Partitions(app)
	if err != nil {
		return err
	}
	today := Today(now)
	var errs []error
	for _, partition := range partitions {
		if err := s.recover(partition.Dir); err != nil {
			errs = append(errs, err)
			continue
		}
		if partition.Date >= today {
			continue
		}
		// recover may have removed files, so list them again.
		files, err := parquetFiles(partition.Dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		partition.Files = files
		if len(files) == 0 || (partition.Compact() && s.hasIndex(app, partition.NS, partition.Date)) {
			continue
		}
		if err := s.compact(app, partition); err != nil {
			errs = append(errs, fmt.Errorf("compact %s %s/%s: %w", app, partition.NS, partition.Date, err))
		}
	}
	return errors.Join(errs...)
}

// compact merges a day's files into one, dropping repeated eids (a batch re-read after a crash
// between write and offset save) and sorting by (tenant_id, event, ts) so min/max statistics skip
// row groups for tenant and event filters. The indexes are written from the same rows.
func (s *Store) compact(app string, partition Partition) error {
	var rows []Row
	for _, file := range partition.Files {
		batch, err := readParquet[Row](file)
		if err != nil {
			return err
		}
		rows = append(rows, batch...)
	}
	rows = Dedupe(rows)
	slices.SortStableFunc(rows, func(a, b Row) int {
		return cmp.Or(strings.Compare(a.TenantID, b.TenantID), strings.Compare(a.Event, b.Event), a.TS.Compare(b.TS))
	})

	if !partition.Compact() {
		target := filepath.Join(partition.Dir, fmt.Sprintf("%s%d%s", dayPrefix, time.Now().UnixNano(), parquetExt))
		temporary := target + ".writing"
		if err := writeParquet(temporary, rows, eventOptions()...); err != nil {
			return err
		}
		if err := fsutil.WriteJSON(filepath.Join(partition.Dir, journalName), journal{Target: target, Sources: partition.Files}, 0o644); err != nil {
			os.Remove(temporary)
			return err
		}
		if err := os.Rename(temporary, target); err != nil {
			return err
		}
		if err := s.recover(partition.Dir); err != nil {
			return err
		}
	}
	return s.writeIndex(app, partition.NS, partition.Date, BuildIndex(partition.NS, rows))
}

// recover finishes a compaction a crash interrupted. A journal whose target exists means the
// merged file is in place, so the sources go; one without its target means the merge never
// landed, so the sources stay and only the leftovers go.
func (s *Store) recover(dir string) error {
	path := filepath.Join(dir, journalName)
	var entry journal
	if err := fsutil.ReadJSON(path, &entry); err != nil {
		return err
	}
	if entry.Target == "" {
		return nil
	}
	if _, err := os.Stat(entry.Target); err == nil {
		for _, source := range entry.Sources {
			if source == entry.Target {
				continue
			}
			if err := os.Remove(source); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	} else {
		os.Remove(entry.Target + ".writing")
	}
	return os.Remove(path)
}

// Dedupe keeps the first row of every eid. A zero eid (a row not written by ingest) is kept.
func Dedupe(rows []Row) []Row {
	seen := make(map[uint64]bool, len(rows))
	out := rows[:0]
	for _, row := range rows {
		if row.EID != 0 {
			if seen[row.EID] {
				continue
			}
			seen[row.EID] = true
		}
		out = append(out, row)
	}
	return out
}

// Prune removes the raw event days of an app older than retention. The indexes stay: they are a
// few rows a day and keep counts and facets for days whose events are gone. A zero or negative
// retention keeps everything; turning events off stops ingest, it does not delete history.
func (s *Store) Prune(app string, retention time.Duration, now time.Time) error {
	if retention <= 0 {
		return nil
	}
	cutoff := now.UTC().Add(-retention).Format(DateLayout)
	partitions, err := s.Partitions(app)
	if err != nil {
		return err
	}
	var errs []error
	for _, partition := range partitions {
		if partition.Date < cutoff {
			errs = append(errs, os.RemoveAll(partition.Dir))
		}
	}
	// A namespace whose days are all gone leaves an empty ns= directory behind.
	namespaces, _ := s.Namespaces(app)
	for _, ns := range namespaces {
		os.Remove(filepath.Join(s.Dir(app), "ns="+ns))
	}
	return errors.Join(errs...)
}
