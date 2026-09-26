package logstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func openExceptionStore(t *testing.T) *Store {
	t.Helper()
	store := New(t.TempDir(), time.Hour, nil, "", time.Hour, 0)
	t.Cleanup(func() { store.Close() })
	return store
}

// exceptionBatch is one group with a single minute row, the shape most tests use.
func singleMinuteBatch(expUID string, minute time.Time, count int) ExceptionBatch {
	return ExceptionBatch{Groups: []ExceptionGroup{{
		ExpUID:  expUID,
		FirstAt: minute,
		LastAt:  minute.Add(30 * time.Second),
		Count:   count,
		Minutes: []ExceptionMinute{{MinuteAt: minute, Count: count, Message: "boom"}},
	}}}
}

func TestAppendExceptionsGroupsPerMinute(t *testing.T) {
	store := openExceptionStore(t)
	base := time.Now().UTC().Truncate(time.Minute).Add(-20 * time.Minute)
	group := ExceptionGroup{ExpUID: "exp-1", Dump: "dump-1", FirstAt: base, LastAt: base.Add(9*time.Minute + 30*time.Second), Count: 44444}
	for i := 0; i < 10; i++ {
		count := 4444
		if i == 9 {
			count = 4448
		}
		group.Minutes = append(group.Minutes, ExceptionMinute{MinuteAt: base.Add(time.Duration(i) * time.Minute), Count: count, Message: "boom"})
	}
	if err := store.AppendExceptions("demo", ExceptionBatch{Groups: []ExceptionGroup{group}}); err != nil {
		t.Fatal(err)
	}

	db := store.apps["demo"].db
	var exceptions, minutes, summed, total int
	var dump string
	if err := db.QueryRow(`SELECT count(*) FROM exceptions`).Scan(&exceptions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM exception_logs`).Scan(&minutes); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT sum(count) FROM exception_logs`).Scan(&summed); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT dump, count FROM exceptions WHERE exp_uid = 'exp-1'`).Scan(&dump, &total); err != nil {
		t.Fatal(err)
	}
	if exceptions != 1 || minutes != 10 || summed != 44444 || total != 44444 || dump != "dump-1" {
		t.Fatalf("exceptions=%d minutes=%d summed=%d total=%d dump=%q", exceptions, minutes, summed, total, dump)
	}
}

func TestExceptionFirstDumpAndMetadataWin(t *testing.T) {
	store := openExceptionStore(t)
	minute := time.Now().UTC().Truncate(time.Minute)

	first := ExceptionBatch{Groups: []ExceptionGroup{{
		ExpUID: "e", FirstAt: minute, LastAt: minute, Count: 1,
		Minutes: []ExceptionMinute{{MinuteAt: minute, Count: 1, Message: "first", Description: "d1", Tags: []string{"a"}}},
	}}}
	second := ExceptionBatch{Groups: []ExceptionGroup{{
		ExpUID: "e", Dump: "late", FirstAt: minute, LastAt: minute, Count: 1,
		Minutes: []ExceptionMinute{{MinuteAt: minute, Count: 1, Message: "second", Description: "d2"}},
	}}}
	if err := store.AppendExceptions("demo", first); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendExceptions("demo", second); err != nil {
		t.Fatal(err)
	}

	db := store.apps["demo"].db
	var dump, message string
	var total int
	if err := db.QueryRow(`SELECT dump, count FROM exceptions WHERE exp_uid = 'e'`).Scan(&dump, &total); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT message FROM exception_logs WHERE exp_uid = 'e' AND minute_at = ?`, minute.UnixMilli()).Scan(&message); err != nil {
		t.Fatal(err)
	}
	if dump != "late" || total != 2 || message != "first" {
		t.Fatalf("dump=%q total=%d message=%q", dump, total, message)
	}

	// A later nonempty dump never replaces the one already saved.
	if err := store.AppendExceptions("demo", ExceptionBatch{Groups: []ExceptionGroup{{ExpUID: "e", Dump: "third", FirstAt: minute, LastAt: minute, Count: 1, Minutes: []ExceptionMinute{{MinuteAt: minute, Count: 1, Message: "x"}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT dump FROM exceptions WHERE exp_uid = 'e'`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if dump != "late" {
		t.Fatalf("dump = %q, want late", dump)
	}
}

func TestExceptionUsersAndIPsDedupeAndCap(t *testing.T) {
	store := openExceptionStore(t)
	minute := time.Now().UTC().Truncate(time.Minute)
	if err := store.AppendExceptions("demo", ExceptionBatch{Groups: []ExceptionGroup{{
		ExpUID: "e", FirstAt: minute, LastAt: minute, Count: 1,
		Minutes: []ExceptionMinute{{MinuteAt: minute, Count: 1, Message: "m",
			Users: []string{"u1", "u2", "u2", "u3"}, IPs: []string{"1.1.1.1", "2.2.2.2"}}},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendExceptions("demo", ExceptionBatch{Groups: []ExceptionGroup{{
		ExpUID: "e", FirstAt: minute, LastAt: minute, Count: 1,
		Minutes: []ExceptionMinute{{MinuteAt: minute, Count: 1, Message: "m",
			Users: []string{"u2", "u4", "u5", "u6", "u7"}, IPs: []string{"3.3.3.3", "4.4.4.4", "5.5.5.5", "6.6.6.6"}}},
	}}}); err != nil {
		t.Fatal(err)
	}

	db := store.apps["demo"].db
	var rawUsers, rawIPs string
	if err := db.QueryRow(`SELECT users, ips FROM exception_logs WHERE exp_uid = 'e' AND minute_at = ?`, minute.UnixMilli()).Scan(&rawUsers, &rawIPs); err != nil {
		t.Fatal(err)
	}
	var users, ips []string
	if err := json.Unmarshal([]byte(rawUsers), &users); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(rawIPs), &ips); err != nil {
		t.Fatal(err)
	}
	wantUsers := []string{"u1", "u2", "u3", "u4", "u5"}
	wantIPs := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"}
	if len(users) != len(wantUsers) {
		t.Fatalf("users = %v", users)
	}
	for i := range wantUsers {
		if users[i] != wantUsers[i] {
			t.Fatalf("users = %v, want %v", users, wantUsers)
		}
	}
	if len(ips) != len(wantIPs) {
		t.Fatalf("ips = %v", ips)
	}
	for i := range wantIPs {
		if ips[i] != wantIPs[i] {
			t.Fatalf("ips = %v, want %v", ips, wantIPs)
		}
	}
}

func TestPruneKeepsExceptionSummary(t *testing.T) {
	store := openExceptionStore(t)
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Minute)
	recent := time.Now().UTC().Truncate(time.Minute)
	group := ExceptionGroup{ExpUID: "e", Dump: "d", FirstAt: old, LastAt: recent, Count: 7,
		Minutes: []ExceptionMinute{
			{MinuteAt: old, Count: 3, Message: "old"},
			{MinuteAt: recent, Count: 4, Message: "recent"},
		}}
	if err := store.AppendExceptions("demo", ExceptionBatch{Groups: []ExceptionGroup{group}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(context.Background(), "demo", time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}

	db := store.apps["demo"].db
	var minutes, total int
	var dump string
	if err := db.QueryRow(`SELECT count(*) FROM exception_logs`).Scan(&minutes); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count, dump FROM exceptions WHERE exp_uid = 'e'`).Scan(&total, &dump); err != nil {
		t.Fatal(err)
	}
	if minutes != 1 || total != 7 || dump != "d" {
		t.Fatalf("minutes=%d total=%d dump=%q", minutes, total, dump)
	}
}

func TestExceptionsReadWithFilter(t *testing.T) {
	store := openExceptionStore(t)
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Minute)
	recent := time.Now().UTC().Truncate(time.Minute)
	if err := store.AppendExceptions("demo", ExceptionBatch{Groups: []ExceptionGroup{
		{ExpUID: "old", FirstAt: old, LastAt: old, Count: 3, Minutes: []ExceptionMinute{{MinuteAt: old, Count: 3, Message: "old"}}},
		{ExpUID: "new", Dump: "dump", FirstAt: recent, LastAt: recent, Count: 5, Minutes: []ExceptionMinute{{MinuteAt: recent, Count: 5, Message: "new", Tags: []string{"a"}, Users: []string{"u1"}, IPs: []string{"1.1.1.1"}}}},
	}}); err != nil {
		t.Fatal(err)
	}

	rows, err := store.Exceptions("demo", ExceptionFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ExpUID != "new" || rows[0].Count != 5 || rows[0].Dump != "dump" {
		t.Fatalf("filtered rows = %+v", rows)
	}
	if len(rows[0].Minutes) != 1 || rows[0].Minutes[0].Message != "new" || rows[0].Minutes[0].Users[0] != "u1" || rows[0].Minutes[0].IPs[0] != "1.1.1.1" {
		t.Fatalf("minutes = %+v", rows[0].Minutes)
	}

	all, err := store.Exceptions("demo", ExceptionFilter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("unfiltered rows: %v %+v", err, all)
	}
	empty, err := store.Exceptions("absent", ExceptionFilter{})
	if err != nil || len(empty) != 0 {
		t.Fatalf("a missing app should read empty: %v %+v", err, empty)
	}
}

func TestSetExceptionResolvedAndMinuteCap(t *testing.T) {
	store := openExceptionStore(t)
	base := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Hour)
	group := ExceptionGroup{ExpUID: "e", FirstAt: base, LastAt: base.Add((ExceptionMinuteLimit + 4) * time.Minute)}
	for i := 0; i < ExceptionMinuteLimit+5; i++ {
		group.Minutes = append(group.Minutes, ExceptionMinute{MinuteAt: base.Add(time.Duration(i) * time.Minute), Count: 1, Message: "m"})
		group.Count++
	}
	if err := store.AppendExceptions("demo", ExceptionBatch{Groups: []ExceptionGroup{group}}); err != nil {
		t.Fatal(err)
	}

	rows, err := store.Exceptions("demo", ExceptionFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows: %v %+v", err, rows)
	}
	if len(rows[0].Minutes) != ExceptionMinuteLimit {
		t.Fatalf("minutes = %d, want %d (the newest ones)", len(rows[0].Minutes), ExceptionMinuteLimit)
	}
	if rows[0].IsResolved {
		t.Fatal("a fresh group is unresolved")
	}

	if err := store.SetExceptionResolved("demo", "e", true); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Exceptions("demo", ExceptionFilter{})
	if err != nil || !rows[0].IsResolved {
		t.Fatalf("resolve did not stick: %v %+v", err, rows)
	}
	if err := store.SetExceptionResolved("demo", "missing", true); err == nil {
		t.Fatal("resolving an unknown fingerprint should fail")
	}
}

func TestResolvedLiftsUnlessIgnored(t *testing.T) {
	store := openExceptionStore(t)
	minute := time.Now().UTC().Truncate(time.Minute)
	if err := store.AppendExceptions("demo", singleMinuteBatch("e", minute, 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetExceptionResolved("demo", "e", true); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendExceptions("demo", singleMinuteBatch("e", minute.Add(time.Minute), 1)); err != nil {
		t.Fatal(err)
	}
	row := mustException(t, store, "e")
	if row.IsResolved || row.IsIgnored || row.Count != 2 {
		t.Fatalf("a repeat should reopen a resolved group: %+v", row)
	}

	if err := store.SetExceptionIgnored("demo", "e", true); err != nil {
		t.Fatal(err)
	}
	row = mustException(t, store, "e")
	if !row.IsResolved || !row.IsIgnored {
		t.Fatalf("ignore should resolve the group: %+v", row)
	}
	count, err := store.UnresolvedExceptionCount("demo")
	if err != nil || count != 0 {
		t.Fatalf("ignored group still counts as unresolved: %d %v", count, err)
	}
	if err := store.AppendExceptions("demo", singleMinuteBatch("e", minute.Add(2*time.Minute), 1)); err != nil {
		t.Fatal(err)
	}
	row = mustException(t, store, "e")
	if !row.IsResolved || !row.IsIgnored || row.Count != 3 {
		t.Fatalf("a repeat should leave an ignored group resolved: %+v", row)
	}

	if err := store.SetExceptionIgnored("demo", "e", false); err != nil {
		t.Fatal(err)
	}
	row = mustException(t, store, "e")
	if !row.IsResolved || row.IsIgnored {
		t.Fatalf("unignore should keep the group resolved: %+v", row)
	}
	if err := store.AppendExceptions("demo", singleMinuteBatch("e", minute.Add(3*time.Minute), 1)); err != nil {
		t.Fatal(err)
	}
	row = mustException(t, store, "e")
	if row.IsResolved || row.IsIgnored || row.Count != 4 {
		t.Fatalf("a repeat after unignore should reopen the group: %+v", row)
	}

	if err := store.SetExceptionIgnored("demo", "e", true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetExceptionResolved("demo", "e", false); err != nil {
		t.Fatal(err)
	}
	row = mustException(t, store, "e")
	if row.IsResolved || row.IsIgnored {
		t.Fatalf("reopen should clear both flags: %+v", row)
	}
	if err := store.SetExceptionIgnored("demo", "missing", true); err == nil {
		t.Fatal("ignoring an unknown fingerprint should fail")
	}
}

func mustException(t *testing.T, store *Store, uid string) ExceptionSummary {
	t.Helper()
	rows, err := store.Exceptions("demo", ExceptionFilter{ExpUID: uid})
	if err != nil || len(rows) != 1 {
		t.Fatalf("exception %s: %v %+v", uid, err, rows)
	}
	return rows[0]
}

func TestExceptionsFilterByUID(t *testing.T) {
	store := openExceptionStore(t)
	minute := time.Now().UTC().Truncate(time.Minute)
	if err := store.AppendExceptions("demo", ExceptionBatch{Groups: []ExceptionGroup{
		{ExpUID: "a", Dump: "dump-a", FirstAt: minute, LastAt: minute, Count: 1, Minutes: []ExceptionMinute{{MinuteAt: minute, Count: 1, Message: "a"}}},
		{ExpUID: "b", FirstAt: minute, LastAt: minute, Count: 2, Minutes: []ExceptionMinute{{MinuteAt: minute, Count: 2, Message: "b"}}},
	}}); err != nil {
		t.Fatal(err)
	}

	rows, err := store.Exceptions("demo", ExceptionFilter{ExpUID: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ExpUID != "b" || rows[0].Count != 2 || rows[0].Dump != "" {
		t.Fatalf("uid filter = %+v", rows)
	}
	if len(rows[0].Minutes) != 1 || rows[0].Minutes[0].Message != "b" {
		t.Fatalf("uid filter minutes = %+v", rows[0].Minutes)
	}

	// A missing fingerprint is an empty list, not an error.
	if rows, err = store.Exceptions("demo", ExceptionFilter{ExpUID: "missing"}); err != nil || len(rows) != 0 {
		t.Fatalf("missing uid = %+v, err %v", rows, err)
	}
}

func TestUnresolvedExceptionCount(t *testing.T) {
	store := openExceptionStore(t)
	minute := time.Now().UTC().Truncate(time.Minute)
	if err := store.AppendExceptions("demo", ExceptionBatch{Groups: []ExceptionGroup{
		{ExpUID: "a", FirstAt: minute, LastAt: minute, Count: 1, Minutes: []ExceptionMinute{{MinuteAt: minute, Count: 1, Message: "a"}}},
		{ExpUID: "b", FirstAt: minute, LastAt: minute, Count: 1, Minutes: []ExceptionMinute{{MinuteAt: minute, Count: 1, Message: "b"}}},
	}}); err != nil {
		t.Fatal(err)
	}

	count, err := store.UnresolvedExceptionCount("demo")
	if err != nil || count != 2 {
		t.Fatalf("count = %d, err %v, want 2", count, err)
	}
	if err := store.SetExceptionResolved("demo", "a", true); err != nil {
		t.Fatal(err)
	}
	if count, err = store.UnresolvedExceptionCount("demo"); err != nil || count != 1 {
		t.Fatalf("count after resolve = %d, err %v, want 1", count, err)
	}
	// An app that never logged has no database and reads as zero, not an error.
	if count, err = store.UnresolvedExceptionCount("absent"); err != nil || count != 0 {
		t.Fatalf("missing app = %d, err %v, want 0", count, err)
	}
}

func TestAppendExceptionsRollsBack(t *testing.T) {
	dir := t.TempDir()
	store := New(dir, time.Hour, nil, "", time.Hour, 0)
	defer store.Close()
	w, err := store.writer("demo")
	if err != nil {
		t.Fatal(err)
	}
	// A closed database makes the whole transaction fail.
	if err := w.db.Close(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute)
	batch := singleMinuteBatch("e", base, 3)
	batch.Path = "/tmp/app.exceptions.log"
	batch.Inode = 7
	batch.Offset = 42
	batch.Warnings = []LogEntry{{Time: base, Source: "file", Process: "app.exceptions.log", Stream: "combined", Level: "warn", Message: "bad line"}}
	if err := store.AppendExceptions("demo", batch); err == nil {
		t.Fatal("AppendExceptions should fail on a closed database")
	}

	fresh := New(dir, time.Hour, nil, "", time.Hour, 0)
	defer fresh.Close()
	if _, err := fresh.writer("demo"); err != nil {
		t.Fatal(err)
	}
	db := fresh.apps["demo"].db
	var exceptions, logs, offsets int
	if err := db.QueryRow(`SELECT count(*) FROM exceptions`).Scan(&exceptions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM logs`).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM tail_offsets`).Scan(&offsets); err != nil {
		t.Fatal(err)
	}
	if exceptions != 0 || logs != 0 || offsets != 0 {
		t.Fatalf("rollback left rows: exceptions=%d logs=%d offsets=%d", exceptions, logs, offsets)
	}
}
