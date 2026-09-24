package events

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseFilterKinds(t *testing.T) {
	filter, err := ParseFilter(`checkout_completed plan:pro,team -#internal value>=10 data.items>3 user=u_42 plan: -"time out" since=7d`)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []TermKind{EventTerm, TagTerm, LabelTerm, ValueTerm, DataTerm, FieldTerm, TagKeyTerm, TextTerm}
	if len(filter.Terms) != len(kinds) {
		t.Fatalf("terms = %+v", filter.Terms)
	}
	for i, kind := range kinds {
		if filter.Terms[i].Kind != kind {
			t.Errorf("term %d kind = %v, want %v", i, filter.Terms[i].Kind, kind)
		}
	}
	if !filter.Terms[2].Not || !filter.Terms[7].Not || filter.Terms[0].Not {
		t.Errorf("negation = %+v", filter.Terms)
	}
	if filter.Terms[7].Values[0] != "time out" {
		t.Errorf("text = %q", filter.Terms[7].Values[0])
	}
	if filter.Since != 7*24*time.Hour {
		t.Errorf("since = %v", filter.Since)
	}
	if got := filter.Terms[1].Values; !slices.Equal(got, []string{"pro", "team"}) {
		t.Errorf("tag values = %v", got)
	}
}

func TestParseFilterRejects(t *testing.T) {
	for _, text := range []string{
		"page_view signup", // two event names
		"value>abc",        // not a number
		"colour=red",       // unknown field
		"user>3",           // ordering on a text field
		`"unclosed`,        // quote
		"-since=7d",        // negated range
		"since=forever",    // duration
		":pro",             // tag without key
		"data.a-b=1",       // path
		"*",                // empty prefix
		"from=yesterday",   // date
	} {
		if _, err := ParseFilter(text); err == nil {
			t.Errorf("%q parsed without error", text)
		}
	}
	// Negated event names may repeat.
	if _, err := ParseFilter("-page_view -signup"); err != nil {
		t.Errorf("negations: %v", err)
	}
}

func TestWhereEscapesLiterals(t *testing.T) {
	filter, err := ParseFilter(`user=o'brien "it's"`)
	if err != nil {
		t.Fatal(err)
	}
	where := filter.Where()
	if !strings.Contains(where, `user_id = 'o''brien'`) || !strings.Contains(where, `'it''s'`) {
		t.Fatalf("where = %s", where)
	}
}

func fixture() []Row {
	value := func(v float64) *float64 { return &v }
	base := now.Add(-2 * time.Hour)
	return []Row{
		{EID: 1, TS: base, Event: "page_view", UserID: "u1", Tags: []string{"page:pricing"}, Msg: "Pricing opened"},
		{EID: 2, TS: base.Add(time.Minute), Event: "checkout_started", UserID: "u1", Tags: []string{"beta", "plan:pro"}, Value: value(20), Data: `{"items":3,"coupon":"X"}`},
		{EID: 3, TS: base.Add(2 * time.Minute), Event: "checkout_completed", UserID: "u1", Tags: []string{"plan:pro"}, Value: value(20), TenantID: "s1", Country: "HR"},
		{EID: 4, TS: base, Event: "page_view", UserID: "u2", Tags: []string{"page:pricing"}},
		{EID: 5, TS: base.Add(time.Minute), Event: "checkout_started", UserID: "u2", Tags: []string{"plan:team", "internal"}, Value: value(5), Data: `{"items":1}`, Msg: "Timeout at payment"},
		{EID: 6, TS: base, Event: "page_view", AnonID: "a1", Tags: []string{"page:home"}},
		{EID: 7, TS: now.Add(-10 * 24 * time.Hour), Event: "page_view", UserID: "u3", Tags: []string{"page:pricing"}},
	}
}

var parityFilters = []string{
	"",
	"page_view",
	"checkout_*",
	"page_view,checkout_started",
	"-page_view",
	"plan:pro",
	"plan:pro,team",
	"plan:",
	"-plan:",
	"#beta",
	"-#internal",
	"value>10",
	"value<=5",
	"data.items>=3",
	"data.coupon=X",
	"-data.coupon=X",
	"user=u1,u2",
	"tenant=s1",
	"country=HR",
	`"timeout"`,
	`-"timeout"`,
	"since=1d",
	"page_view since=30d",
	"ns=shop",
	"ns=other",
}

func matchGo(t *testing.T, filter Filter, rows []Row) []uint64 {
	t.Helper()
	var out []uint64
	for _, row := range rows {
		if filter.Match(row, "shop", time.Now()) {
			out = append(out, row.EID)
		}
	}
	return out
}

// testDuckDB finds duckdb on PATH or skips; the SQL half of the filter is checked against it.
func testDuckDB(t *testing.T) DuckDB {
	t.Helper()
	duck, err := FindDuckDB(context.Background())
	if err != nil {
		t.Skip(err)
	}
	return duck
}

func TestFilterSQLMatchesGo(t *testing.T) {
	duck := testDuckDB(t)
	store := NewStore(t.TempDir())
	// Timestamps relative to the real clock, since since= compiles to now() in SQL.
	rows := fixture()
	shift := time.Since(now)
	for i := range rows {
		rows[i].TS = rows[i].TS.Add(shift).Truncate(time.Millisecond)
	}
	if err := store.Append("demo", "shop", rows); err != nil {
		t.Fatal(err)
	}
	views, err := store.BaseViews("demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range parityFilters {
		filter, err := ParseFilter(text)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		want := matchGo(t, filter, rows)
		result, err := duck.Query(context.Background(), store.Dir("demo"), views, "SELECT eid FROM events WHERE "+filter.Where()+" ORDER BY eid")
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		var got []uint64
		for _, row := range result.Rows {
			var eid uint64
			fmt.Sscan(fmt.Sprint(row[0]), &eid)
			got = append(got, eid)
		}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%q: sql %v, go %v\n%s", text, got, want, filter.Where())
		}
	}
}

func TestFunnelSQL(t *testing.T) {
	duck := testDuckDB(t)
	store := NewStore(t.TempDir())
	if err := store.Append("demo", "shop", fixture()); err != nil {
		t.Fatal(err)
	}
	views, err := store.BaseViews("demo")
	if err != nil {
		t.Fatal(err)
	}
	funnel := Funnel{Name: "checkout", Breakdown: "plan", Steps: []Step{
		{Name: "Pricing", Filter: "page_view page:pricing"},
		{Name: "Started", Filter: "checkout_started"},
		{Name: "Paid", Filter: "checkout_completed"},
	}}
	query, err := FunnelSQL(Funnel{Name: funnel.Name, Steps: funnel.Steps}, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := duck.Query(context.Background(), store.Dir("demo"), views, query)
	if err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
	// u1, u2, u3 viewed pricing; u1 and u2 started within the window (u3 never did); u1 paid.
	actors := []string{}
	for _, row := range result.Rows {
		actors = append(actors, fmt.Sprint(row[3]))
	}
	if !slices.Equal(actors, []string{"3", "2", "1"}) {
		t.Fatalf("actors = %v\n%v", actors, result.Rows)
	}
	if fmt.Sprint(result.Rows[1][4]) != "66.7" || fmt.Sprint(result.Rows[2][5]) != "50.0" {
		t.Fatalf("pct = %v", result.Rows)
	}

	// Broken down by the plan tag of step 1: the pricing views carry no plan, so all rows share
	// one null breakdown; by data.items the start step's value does not matter either.
	query, err = FunnelSQL(funnel, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := duck.Query(context.Background(), store.Dir("demo"), views, query); err != nil {
		t.Fatalf("breakdown: %v\n%s", err, query)
	}

	// A saved view and a funnel view load together with the base views.
	saved := SavedViews([]View{{Name: "pro", Filter: "plan:pro"}}, []Funnel{funnel})
	result, err = duck.Query(context.Background(), store.Dir("demo"), views+saved, "SELECT count(*) FROM pro")
	if err != nil || fmt.Sprint(result.Rows[0][0]) != "2" {
		t.Fatalf("saved view: %v %v", result, err)
	}
}

func TestQuerySandbox(t *testing.T) {
	duck := testDuckDB(t)
	store := NewStore(t.TempDir())
	store.Append("demo", "shop", fixture())
	views, _ := store.BaseViews("demo")
	for _, sql := range []string{
		"SELECT * FROM read_csv('/etc/passwd')",
		"ATTACH '/tmp/dboss-events-test.db'",
		"SET enable_external_access = true",
		"INSTALL httpfs",
	} {
		if _, err := duck.Query(context.Background(), store.Dir("demo"), views, sql); err == nil {
			t.Errorf("%s ran inside the sandbox", sql)
		}
	}
}

func TestEmptyAppViews(t *testing.T) {
	duck := testDuckDB(t)
	store := NewStore(t.TempDir())
	views, err := store.BaseViews("demo")
	if err != nil {
		t.Fatal(err)
	}
	result, err := duck.Query(context.Background(), store.Dir("demo"), views, "SELECT (SELECT count(*) FROM events) + (SELECT count(*) FROM events_daily) AS n")
	if err != nil || fmt.Sprint(result.Rows[0][0]) != "0" {
		t.Fatalf("empty views: %v %v", result, err)
	}
}

func TestFunnelJSONWindow(t *testing.T) {
	funnel := Funnel{Name: "f", Window: 7 * 24 * time.Hour, Steps: []Step{{Filter: "a"}, {Filter: "b"}}}
	data, err := json.Marshal(funnel)
	if err != nil || !strings.Contains(string(data), `"window":"7d"`) {
		t.Fatalf("marshal = %s, %v", data, err)
	}
	var back Funnel
	if err := json.Unmarshal(data, &back); err != nil || back.Window != funnel.Window || len(back.Steps) != 2 {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"name":"f","window":"36h"}`), &back); err != nil || back.Window != 36*time.Hour {
		t.Fatalf("36h = %+v, %v", back, err)
	}
}
