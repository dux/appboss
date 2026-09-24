package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// View is a saved filter; DuckDB sees it as a view over events.
type View struct {
	Name   string `json:"name" yaml:"-"`
	Filter string `json:"filter"`
	Source string `json:"source"` // yaml or console
}

// Funnel is a saved funnel: the actors (users, anonymous visitors or tenants) that did each step
// in order, within Window of their first step.
type Funnel struct {
	Name      string        `json:"name"`
	By        string        `json:"by"` // user, anon or tenant
	Window    time.Duration `json:"window"`
	Breakdown string        `json:"breakdown,omitempty"` // a field, a tag key or data.<path>
	Steps     []Step        `json:"steps"`
	Source    string        `json:"source"` // yaml or console
}

// MarshalJSON writes the window the way dboss.yaml does (7d, 36h) instead of nanoseconds.
func (f Funnel) MarshalJSON() ([]byte, error) {
	type plain Funnel
	return json.Marshal(struct {
		plain
		Window string `json:"window"`
	}{plain(f), FormatDuration(f.Window)})
}

// UnmarshalJSON reads the window as a duration string (7d, 36h) or nanoseconds.
func (f *Funnel) UnmarshalJSON(data []byte) error {
	type plain Funnel
	var decoded struct {
		plain
		Window json.RawMessage `json:"window"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*f = Funnel(decoded.plain)
	f.Window = 0
	if len(decoded.Window) == 0 || string(decoded.Window) == "null" {
		return nil
	}
	var text string
	if json.Unmarshal(decoded.Window, &text) == nil {
		if text == "" {
			return nil
		}
		window, err := ParseDuration(text)
		if err != nil {
			return fmt.Errorf("window %q: use a duration like 7d or 36h", text)
		}
		f.Window = window
		return nil
	}
	var nanos int64
	if err := json.Unmarshal(decoded.Window, &nanos); err != nil {
		return errors.New("window: use a duration like 7d or 36h")
	}
	f.Window = time.Duration(nanos)
	return nil
}

// FormatDuration writes whole days as Nd and anything else as a Go duration.
func FormatDuration(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}

// Step is one funnel step.
type Step struct {
	Name   string `json:"name"`
	Filter string `json:"filter"`
}

// Sources of a saved view or funnel. The app's dboss.yaml wins over one saved from the console
// under the same name.
const (
	SourceYAML    = "yaml"
	SourceConsole = "console"
)

// Funnel limits.
const (
	MinSteps      = 2
	MaxSteps      = 10
	DefaultWindow = 7 * 24 * time.Hour
)

var namePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// actorColumns maps a funnel's by to the column that identifies an actor.
var actorColumns = map[string]string{"user": "user_id", "anon": "anon_id", "tenant": "tenant_id"}

// reservedNames are the base views; a saved view cannot shadow them.
var reservedNames = []string{"events", "events_daily", "facets", "data_keys"}

// ValidateView checks a saved view's name and filter.
func ValidateView(view View) error {
	if err := validateName(view.Name); err != nil {
		return err
	}
	if _, err := ParseFilter(view.Filter); err != nil {
		return fmt.Errorf("view %s: %w", view.Name, err)
	}
	return nil
}

// ValidateFunnel checks a funnel and fills its defaults (by user, a 7 day window).
func ValidateFunnel(funnel *Funnel) error {
	if err := validateName(funnel.Name); err != nil {
		return err
	}
	if funnel.By == "" {
		funnel.By = "user"
	}
	if _, ok := actorColumns[funnel.By]; !ok {
		return fmt.Errorf("funnel %s: by is user, anon or tenant", funnel.Name)
	}
	if funnel.Window == 0 {
		funnel.Window = DefaultWindow
	}
	if funnel.Window < 0 {
		return fmt.Errorf("funnel %s: window must be positive", funnel.Name)
	}
	if len(funnel.Steps) < MinSteps || len(funnel.Steps) > MaxSteps {
		return fmt.Errorf("funnel %s: needs %d to %d steps", funnel.Name, MinSteps, MaxSteps)
	}
	for i, step := range funnel.Steps {
		if strings.TrimSpace(step.Filter) == "" {
			return fmt.Errorf("funnel %s step %d: a step needs a filter", funnel.Name, i+1)
		}
		if _, err := ParseFilter(step.Filter); err != nil {
			return fmt.Errorf("funnel %s step %d: %w", funnel.Name, i+1, err)
		}
	}
	if _, err := breakdownSQL(funnel.Breakdown); err != nil {
		return fmt.Errorf("funnel %s: %w", funnel.Name, err)
	}
	return nil
}

func validateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("name %q: use 1-64 lowercase letters, digits and _", name)
	}
	if slices.Contains(reservedNames, name) {
		return fmt.Errorf("name %q is a built-in view", name)
	}
	return nil
}

// breakdownSQL is the expression a funnel groups by: a fixed field, data.<path> or a tag key.
func breakdownSQL(breakdown string) (string, error) {
	switch {
	case breakdown == "":
		return "NULL", nil
	case breakdown == "event":
		return "event", nil
	case strings.HasPrefix(breakdown, "data."):
		path := strings.TrimPrefix(breakdown, "data.")
		if !dataPathPattern.MatchString(path) {
			return "", errors.New("breakdown: a data path is letters, digits and _ separated by dots")
		}
		return fmt.Sprintf("json_extract_string(data, %s)", quote("$."+path)), nil
	}
	if column, ok := fieldColumns[breakdown]; ok {
		return column, nil
	}
	if strings.ContainsAny(breakdown, " :#,") {
		return "", errors.New("breakdown is a field (ns, user, anon, tenant, country, req, event), data.<key> or a tag key")
	}
	prefix := quote(breakdown + ":")
	return fmt.Sprintf("split_part(list_filter(tags, lambda t: starts_with(t, %s))[1], ':', 2)", prefix), nil
}

// FunnelSQL is the query of a funnel. Step N is the first event matching filter N at or after the
// actor's step N-1 and within Window of their step 1; extra (a time range from the console) only
// narrows step 1. It returns one row per step and breakdown value with the actors that reached
// it, the share of step 1 and of the previous step, and the median seconds from the previous step.
func FunnelSQL(funnel Funnel, extra Filter) (string, error) {
	if err := ValidateFunnel(&funnel); err != nil {
		return "", err
	}
	actor := actorColumns[funnel.By]
	breakdown, _ := breakdownSQL(funnel.Breakdown)
	window := fmt.Sprintf("to_seconds(%d)", int64(funnel.Window.Seconds()))
	var ctes, selects []string
	for i, step := range funnel.Steps {
		filter, _ := ParseFilter(step.Filter)
		where := filter.Where()
		n := i + 1
		if i == 0 {
			where += " AND " + extra.Where()
			ctes = append(ctes, fmt.Sprintf("s1 AS (SELECT %s AS actor, arg_min(%s, ts) AS bd, min(ts) AS t1, min(ts) AS t, NULL::TIMESTAMPTZ AS prev FROM events WHERE %s IS NOT NULL AND (%s) GROUP BY 1)", actor, breakdown, actor, where))
		} else {
			// The filter names event columns unqualified; the CTE's own columns (actor, bd, t1, t,
			// prev) never collide with them, so they resolve to the joined events row e.
			ctes = append(ctes, fmt.Sprintf("s%d AS (SELECT p.actor, p.bd, p.t1, min(e.ts) AS t, p.t AS prev FROM s%d p JOIN events e ON e.%s = p.actor AND e.ts >= p.t AND e.ts <= p.t1 + %s WHERE %s GROUP BY p.actor, p.bd, p.t1, p.t)", n, i, actor, window, where))
		}
		selects = append(selects, fmt.Sprintf("SELECT %d AS step, %s AS name, bd AS breakdown, count(*) AS actors, median(epoch(t) - epoch(prev)) AS median_seconds FROM s%d GROUP BY bd", n, quote(step.Name), n))
	}
	return fmt.Sprintf(`WITH %s,
steps AS (%s)
SELECT step, name, breakdown, actors,
  round(100.0 * actors / first_value(actors) OVER (PARTITION BY breakdown ORDER BY step), 1) AS pct_of_first,
  round(100.0 * actors / lag(actors) OVER (PARTITION BY breakdown ORDER BY step), 1) AS pct_of_previous,
  round(median_seconds) AS median_seconds
FROM steps ORDER BY breakdown NULLS FIRST, step`, strings.Join(ctes, ",\n"), strings.Join(selects, "\nUNION ALL\n")), nil
}

// BaseViews is the DuckDB view SQL over one app's files. Globs over a directory with no files
// fail in DuckDB, so a dataset with nothing on disk yet becomes an empty view of the same shape.
func (s *Store) BaseViews(app string) (string, error) {
	dir := s.Dir(app)
	partitions, err := s.Partitions(app)
	if err != nil {
		return "", err
	}
	hasEvents := slices.ContainsFunc(partitions, func(p Partition) bool { return len(p.Files) > 0 })
	dates, err := s.IndexDates(app)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	view := func(name, glob, empty, replace string) {
		fmt.Fprintf(&out, "CREATE OR REPLACE VIEW %s AS ", name)
		if glob == "" {
			fmt.Fprintf(&out, "SELECT * FROM (SELECT %s) WHERE false;\n", empty)
			return
		}
		fmt.Fprintf(&out, "SELECT *%s FROM read_parquet(%s, hive_partitioning = true, union_by_name = true);\n", replace, quote(glob))
	}
	eventsGlob, indexGlob := "", func(string) string { return "" }
	if hasEvents {
		eventsGlob = dir + "/ns=*/*/*.parquet"
	}
	if len(dates) > 0 {
		indexGlob = func(index string) string { return dir + "/" + index + "/*/*.parquet" }
	}
	view("events", eventsGlob, emptyEvents, " REPLACE (data::JSON AS data)")
	view("events_daily", indexGlob(DailyIndex), "NULL::VARCHAR AS ns, NULL::VARCHAR AS event, 0::BIGINT AS count, 0::BIGINT AS users, 0::BIGINT AS anons, 0::DOUBLE AS value_sum, NULL::DATE AS date", "")
	view("facets", indexGlob(FacetIndex), "NULL::VARCHAR AS ns, NULL::VARCHAR AS event, NULL::VARCHAR AS key, NULL::VARCHAR AS value, 0::BIGINT AS count, NULL::DATE AS date", "")
	view("data_keys", indexGlob(KeyIndex), "NULL::VARCHAR AS ns, NULL::VARCHAR AS event, NULL::VARCHAR AS path, NULL::VARCHAR AS type, 0::BIGINT AS count, NULL::DATE AS date", "")
	return out.String(), nil
}

const emptyEvents = "0::UBIGINT AS eid, NULL::TIMESTAMPTZ AS ts, NULL::VARCHAR AS event, NULL::VARCHAR AS msg, []::VARCHAR[] AS tags, " +
	"NULL::VARCHAR AS user_id, NULL::VARCHAR AS anon_id, NULL::VARCHAR AS tenant_id, NULL::VARCHAR AS request_id, " +
	"NULL::DOUBLE AS value, NULL::VARCHAR AS country, NULL::JSON AS data, NULL::DATE AS date, NULL::VARCHAR AS ns"

// SavedViews is the view SQL of saved filters and funnels, on top of BaseViews. A funnel becomes
// a view named funnel_<name>.
func SavedViews(views []View, funnels []Funnel) string {
	var out strings.Builder
	for _, view := range views {
		filter, err := ParseFilter(view.Filter)
		if err != nil {
			continue
		}
		fmt.Fprintf(&out, "CREATE OR REPLACE VIEW %s AS SELECT * FROM events WHERE %s;\n", view.Name, filter.Where())
	}
	for _, funnel := range funnels {
		query, err := FunnelSQL(funnel, Filter{})
		if err != nil {
			continue
		}
		fmt.Fprintf(&out, "CREATE OR REPLACE VIEW funnel_%s AS %s;\n", funnel.Name, query)
	}
	return out.String()
}
