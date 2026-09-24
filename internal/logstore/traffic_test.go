package logstore

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTrafficAggregatesTheRequestLog(t *testing.T) {
	store := New(t.TempDir(), 5*time.Millisecond, nil, "", time.Hour, 0)
	defer store.Close()

	now := time.Now().UTC()
	record := func(age time.Duration, path string, status int, ms int64, ip, country string) {
		t.Helper()
		entry := RequestEntry{Time: now.Add(-age), Method: "GET", Host: "demo.test", Path: path, Status: status, DurationMS: ms, BytesOut: 10, IP: ip, Country: country}
		if err := store.Record("demo", 24*time.Hour, entry); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 6; i++ {
		record(time.Minute, "/", 200, 10, "1.1.1.1", "HR")
	}
	for i := 0; i < 5; i++ {
		record(10*time.Minute, "/report", 200, 900, "2.2.2.2", "DE")
	}
	record(10*time.Minute, "/missing", 404, 5, "2.2.2.2", "DE")
	record(10*time.Minute, "/boom", 502, 4000, "3.3.3.3", "")
	record(3*time.Hour, "/old", 500, 1, "9.9.9.9", "US")
	waitFor(t, func() ([]RequestEntry, error) {
		rows, err := store.SearchRequests("demo", RequestFilter{})
		if len(rows) < 14 {
			return nil, err
		}
		return rows, err
	})

	traffic, err := store.Traffic("demo", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if traffic.Totals.Count != 13 || traffic.Totals.Errors != 1 || traffic.Bucket != "minute" {
		t.Fatalf("unexpected totals: %+v bucket %s", traffic.Totals, traffic.Bucket)
	}
	if len(traffic.Series) < 60 || len(traffic.Series) > 62 {
		t.Fatalf("series must cover every minute of the hour, got %d", len(traffic.Series))
	}
	var s2, s4, s5 int64
	for _, bucket := range traffic.Series {
		s2, s4, s5 = s2+bucket.S2, s4+bucket.S4, s5+bucket.S5
	}
	if s2 != 11 || s4 != 1 || s5 != 1 {
		t.Fatalf("series totals: s2=%d s4=%d s5=%d", s2, s4, s5)
	}
	if got := traffic.Paths[0]; got.Path != "/" || got.Count != 6 {
		t.Fatalf("top path: %+v", got)
	}
	// /boom is slower but was hit once, so it stays out of the slowest list.
	if len(traffic.Slowest) != 2 || traffic.Slowest[0].Path != "/report" || traffic.Slowest[0].AvgMS != 900 {
		t.Fatalf("slowest: %+v", traffic.Slowest)
	}
	if fmt.Sprint(traffic.Statuses) != "[{200 11} {404 1} {502 1}]" {
		t.Fatalf("statuses: %+v", traffic.Statuses)
	}
	if fmt.Sprint(traffic.Countries) != "[{DE 6} {HR 6}]" {
		t.Fatalf("countries skip the empty value: %+v", traffic.Countries)
	}
	if traffic.IPs[0].Key != "1.1.1.1" && traffic.IPs[0].Key != "2.2.2.2" || len(traffic.IPs) != 3 {
		t.Fatalf("ips: %+v", traffic.IPs)
	}

	if day, _ := store.Traffic("demo", now.Add(-24*time.Hour)); day.Bucket != "hour" || day.Totals.Count != 14 {
		t.Fatalf("24h range: bucket %s totals %+v", day.Bucket, day.Totals)
	}
}

func TestTrafficNeverCreatesADatabase(t *testing.T) {
	dir := t.TempDir()
	store := New(dir, 5*time.Millisecond, nil, "", time.Hour, 0)
	defer store.Close()

	traffic, err := store.Traffic("quiet", time.Now().Add(-time.Hour))
	if err != nil || traffic.Totals.Count != 0 || traffic.Paths == nil || len(traffic.Series) == 0 {
		t.Fatalf("app without a database: %v %+v", err, traffic)
	}
	if _, err := os.Stat(filepath.Join(dir, "quiet")); !os.IsNotExist(err) {
		t.Fatalf("Traffic created %s: %v", filepath.Join(dir, "quiet"), err)
	}
}

func TestSeriesSumsAppsIntoOneGrid(t *testing.T) {
	store := New(t.TempDir(), 5*time.Millisecond, nil, "", time.Hour, 0)
	defer store.Close()

	now := time.Now().UTC()
	for app, statuses := range map[string][]int{"one": {200, 200, 500}, "two": {200, 404}} {
		for _, status := range statuses {
			if err := store.Record(app, 24*time.Hour, RequestEntry{Time: now.Add(-time.Minute), Method: "GET", Path: "/", Status: status}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for app, want := range map[string]int{"one": 3, "two": 2} {
		waitFor(t, func() ([]RequestEntry, error) {
			rows, err := store.SearchRequests(app, RequestFilter{})
			if len(rows) < want {
				return nil, err
			}
			return rows, err
		})
	}

	series, err := store.Series([]string{"one", "two", "quiet"}, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(series) < 24 || len(series) > 26 {
		t.Fatalf("series must cover every hour of the day, got %d", len(series))
	}
	var s2, s4, s5 int64
	for _, bucket := range series {
		s2, s4, s5 = s2+bucket.S2, s4+bucket.S4, s5+bucket.S5
	}
	if s2 != 3 || s4 != 1 || s5 != 1 {
		t.Fatalf("series totals: s2=%d s4=%d s5=%d", s2, s4, s5)
	}
}
