package events

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dboss/internal/fsutil"
)

func event(eid uint64, ts time.Time, name, tenant string, tags ...string) Row {
	value := 1.5
	return Row{EID: eid, TS: ts, Event: name, TenantID: tenant, UserID: "u" + tenant, Tags: tags, Value: &value, Data: `{"k":1}`}
}

func TestAppendSplitsByDayAndReadsBack(t *testing.T) {
	store := NewStore(t.TempDir())
	yesterday := now.Add(-24 * time.Hour)
	rows := []Row{event(1, now, "a", "t1", "plan:pro"), event(2, yesterday, "b", "t2")}
	if err := store.Append("demo", "shop", rows); err != nil {
		t.Fatal(err)
	}
	partitions, err := store.Partitions("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(partitions) != 2 || partitions[0].Date != "2026-09-23" || partitions[1].Date != "2026-09-24" {
		t.Fatalf("partitions = %+v", partitions)
	}
	got, err := readParquet[Row](partitions[1].Files[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Event != "a" || got[0].Tags[0] != "plan:pro" || *got[0].Value != 1.5 || !got[0].TS.Equal(now) {
		t.Fatalf("read back %+v", got)
	}
}

func TestCompactDedupesSortsAndIndexes(t *testing.T) {
	store := NewStore(t.TempDir())
	day := now.Add(-24 * time.Hour)
	first := []Row{event(1, day, "b", "t2", "plan:pro"), event(2, day.Add(time.Second), "a", "t1", "beta")}
	// The same batch read twice after a crash between write and offset save.
	if err := store.Append("demo", "shop", first); err != nil {
		t.Fatal(err)
	}
	if err := store.Append("demo", "shop", append(first, event(3, day, "a", "t1", "plan:team"))); err != nil {
		t.Fatal(err)
	}
	// Today stays as it is.
	if err := store.Append("demo", "shop", []Row{event(9, now, "a", "t1")}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactApp("demo", now); err != nil {
		t.Fatal(err)
	}
	partitions, _ := store.Partitions("demo")
	if !partitions[0].Compact() || partitions[1].Compact() {
		t.Fatalf("partitions = %+v", partitions)
	}
	rows, err := readParquet[Row](partitions[0].Files[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 after dedupe", len(rows))
	}
	order := []uint64{3, 2, 1} // t1/a/day, t1/a/day+1s, t2/b
	for i, row := range rows {
		if row.EID != order[i] {
			t.Fatalf("row %d eid = %d, want %d", i, row.EID, order[i])
		}
	}
	index, ok, err := store.readIndex("demo", "shop", "2026-09-23")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if len(index.Daily) != 2 || index.Daily[0].Event != "a" || index.Daily[0].Count != 2 || index.Daily[0].Users != 1 || index.Daily[0].ValueSum != 3 {
		t.Fatalf("daily = %+v", index.Daily)
	}
	if len(index.Facets) != 3 || index.Facets[0] != (FacetRow{NS: "shop", Event: "a", Key: "#", Value: "beta", Count: 1}) {
		t.Fatalf("facets = %+v", index.Facets)
	}
	if len(index.Keys) != 2 || index.Keys[0] != (KeyRow{NS: "shop", Event: "a", Path: "k", Type: "number", Count: 2}) {
		t.Fatalf("keys = %+v", index.Keys)
	}

	// A second run finds nothing to do and keeps the same file.
	before := partitions[0].Files[0]
	if err := store.CompactApp("demo", now); err != nil {
		t.Fatal(err)
	}
	partitions, _ = store.Partitions("demo")
	if partitions[0].Files[0] != before {
		t.Fatal("a compacted day was rewritten")
	}

	// A late event for that day is merged into the day file on the next run.
	if err := store.Append("demo", "shop", []Row{event(4, day, "c", "t3")}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactApp("demo", now); err != nil {
		t.Fatal(err)
	}
	partitions, _ = store.Partitions("demo")
	rows, _ = readParquet[Row](partitions[0].Files[0])
	if !partitions[0].Compact() || len(rows) != 4 {
		t.Fatalf("after late event: %+v, %d rows", partitions[0], len(rows))
	}
}

func TestCompactRecoversInterruptedRun(t *testing.T) {
	store := NewStore(t.TempDir())
	day := now.Add(-24 * time.Hour)
	if err := store.Append("demo", "shop", []Row{event(1, day, "a", "t")}); err != nil {
		t.Fatal(err)
	}
	partitions, _ := store.Partitions("demo")
	source := partitions[0].Files[0]
	// Crash after the merged file landed but before the sources were deleted.
	target := filepath.Join(partitions[0].Dir, "day-1.parquet")
	data, _ := os.ReadFile(source)
	os.WriteFile(target, data, 0o644)
	fsutil.WriteJSON(filepath.Join(partitions[0].Dir, journalName), journal{Target: target, Sources: []string{source}}, 0o644)

	if err := store.CompactApp("demo", now); err != nil {
		t.Fatal(err)
	}
	partitions, _ = store.Partitions("demo")
	if len(partitions[0].Files) != 1 || partitions[0].Files[0] != target {
		t.Fatalf("files = %v, want only %s", partitions[0].Files, target)
	}
	if _, err := os.Stat(filepath.Join(partitions[0].Dir, journalName)); !os.IsNotExist(err) {
		t.Fatal("journal left behind")
	}
}

func TestPruneKeepsIndexes(t *testing.T) {
	store := NewStore(t.TempDir())
	old := now.Add(-40 * 24 * time.Hour)
	store.Append("demo", "shop", []Row{event(1, old, "a", "t"), event(2, now, "a", "t")})
	if err := store.CompactApp("demo", now); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune("demo", 30*24*time.Hour, now); err != nil {
		t.Fatal(err)
	}
	partitions, _ := store.Partitions("demo")
	if len(partitions) != 1 || partitions[0].Date != "2026-09-24" {
		t.Fatalf("partitions = %+v", partitions)
	}
	if dates, _ := store.IndexDates("demo"); len(dates) != 1 || dates[0] != old.Format(DateLayout) {
		t.Fatalf("index dates = %v", dates)
	}
}
