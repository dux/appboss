package events

import (
	"slices"
	"testing"
	"time"
)

// readerFixture stores the fixture with one closed day compacted and today raw.
func readerFixture(t *testing.T) (*Store, *Reader) {
	t.Helper()
	store := NewStore(t.TempDir())
	rows := fixture()
	yesterday := now.Add(-24 * time.Hour)
	rows = append(rows, Row{EID: 8, TS: yesterday, Event: "page_view", UserID: "u9", Tags: []string{"page:pricing", "plan:pro"}})
	if err := store.Append("demo", "shop", rows); err != nil {
		t.Fatal(err)
	}
	if err := store.Append("demo", "billing", []Row{{EID: 9, TS: now.Add(-time.Hour), Event: "invoice", UserID: "u1"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactApp("demo", now); err != nil {
		t.Fatal(err)
	}
	return store, NewReader(store)
}

func mustFilter(t *testing.T, text string) Filter {
	t.Helper()
	filter, err := ParseFilter(text)
	if err != nil {
		t.Fatal(err)
	}
	return filter
}

func TestSummaryFromIndexes(t *testing.T) {
	_, reader := readerFixture(t)
	summary, err := reader.Summary("demo", mustFilter(t, "page_view"), now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Scanned || summary.Count != 5 {
		t.Fatalf("summary = %+v", summary)
	}
	if !slices.Equal(summary.Namespaces, []string{"billing", "shop"}) {
		t.Fatalf("namespaces = %v", summary.Namespaces)
	}
	summary, err = reader.Summary("demo", mustFilter(t, "ns=billing"), now)
	if err != nil || summary.Count != 1 {
		t.Fatalf("ns summary = %+v, %v", summary, err)
	}
}

func TestSummaryScansForTagFilters(t *testing.T) {
	_, reader := readerFixture(t)
	summary, err := reader.Summary("demo", mustFilter(t, "plan:pro"), now)
	if err != nil {
		t.Fatal(err)
	}
	// Events 2 and 3 today, 8 yesterday.
	if !summary.Scanned || summary.Count != 3 || summary.Users != 2 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestLatestNewestFirst(t *testing.T) {
	_, reader := readerFixture(t)
	latest, err := reader.Latest("demo", mustFilter(t, "user=u1"), 3, now)
	if err != nil {
		t.Fatal(err)
	}
	var eids []uint64
	for _, event := range latest {
		eids = append(eids, event.EID)
	}
	if !slices.Equal(eids, []uint64{9, 3, 2}) || latest[0].NS != "billing" {
		t.Fatalf("latest = %v", eids)
	}
}

func TestFacets(t *testing.T) {
	_, reader := readerFixture(t)
	values := func(facets []Facet) []string {
		var out []string
		for _, facet := range facets {
			out = append(out, facet.Value)
		}
		return out
	}
	// Index path: plan values over everything in range.
	facets, err := reader.Facets("demo", Filter{}, "plan", now)
	if err != nil || !slices.Equal(values(facets), []string{"pro", "team"}) || facets[0].Count != 3 {
		t.Fatalf("plan facets = %+v, %v", facets, err)
	}
	// Scan path: only the users' events.
	facets, err = reader.Facets("demo", mustFilter(t, "user=u2"), "plan", now)
	if err != nil || !slices.Equal(values(facets), []string{"team"}) {
		t.Fatalf("u2 plan facets = %+v, %v", facets, err)
	}
	facets, _ = reader.Facets("demo", Filter{}, "", now)
	if !slices.Equal(values(facets), []string{"page", "plan"}) {
		t.Fatalf("tag keys = %+v", facets)
	}
	facets, _ = reader.Facets("demo", Filter{}, "#", now)
	if !slices.Equal(values(facets), []string{"beta", "internal"}) {
		t.Fatalf("labels = %+v", facets)
	}
	facets, _ = reader.Facets("demo", mustFilter(t, "checkout_started"), "data.", now)
	if !slices.Equal(values(facets), []string{"items", "coupon"}) || facets[0].Type != "number" {
		t.Fatalf("data keys = %+v", facets)
	}
}
