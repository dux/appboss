package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Filter is a parsed filter expression. The same text filters the console, `dboss events
// --filter`, funnel steps and saved views:
//
//	checkout_completed plan:pro -#internal value>10 data.items>=3 user=u_42 since=7d "timeout"
//
// Terms separated by spaces are ANDed, a leading - negates a term, commas inside a value OR its
// alternatives. A bare word is the event name, since every event has one. The filter compiles to
// DuckDB SQL (Where) and to a Go predicate (Match) with the same meaning.
type Filter struct {
	Terms []Term
	Since time.Duration // since=7d: the last N before now
	From  time.Time     // from=2026-09-01: inclusive
	To    time.Time     // to=2026-09-30: exclusive (a bare date means the end of that day)
	text  string
}

// Term is one condition. Kind picks which of the other fields apply.
type Term struct {
	Kind   TermKind
	Not    bool
	Field  string   // column for FieldTerm, JSON path for DataTerm, tag key for TagTerm/TagKeyTerm
	Op     string   // =, >, >=, <, <= for ValueTerm and DataTerm
	Values []string // alternatives, ORed
	Number float64  // the operand of a numeric comparison
}

type TermKind int

const (
	EventTerm       TermKind = iota // checkout,signup
	EventPrefixTerm                 // checkout_*
	FieldTerm                       // user=u_42, tenant=, anon=, ns=, country=, req=
	ValueTerm                       // value>10
	TagTerm                         // plan:pro,team
	TagKeyTerm                      // plan:
	LabelTerm                       // #beta
	DataTerm                        // data.coupon=X, data.items>=3
	TextTerm                        // "timeout"
)

// fieldColumns maps the short field names of the filter to their columns.
var fieldColumns = map[string]string{
	"ns":      "ns",
	"user":    "user_id",
	"anon":    "anon_id",
	"tenant":  "tenant_id",
	"country": "country",
	"req":     "request_id",
}

var (
	dataPathPattern = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)*$`)
	operators       = []string{">=", "<=", "=", ">", "<"}
)

// ParseFilter parses a filter expression. An empty one matches everything.
func ParseFilter(text string) (Filter, error) {
	tokens, err := tokenize(text)
	if err != nil {
		return Filter{}, err
	}
	filter := Filter{text: strings.TrimSpace(text)}
	positiveEvents := 0
	for _, token := range tokens {
		term, timed, err := filter.parseToken(token)
		if err != nil {
			return Filter{}, fmt.Errorf("%s: %w", token.display(), err)
		}
		if timed {
			continue
		}
		if (term.Kind == EventTerm || term.Kind == EventPrefixTerm) && !term.Not {
			positiveEvents++
			if positiveEvents > 1 {
				return Filter{}, fmt.Errorf("%s: an event has one name; for several events use a,b", token.display())
			}
		}
		filter.Terms = append(filter.Terms, term)
	}
	return filter, nil
}

// String is the filter as written.
func (f Filter) String() string { return f.text }

type token struct {
	text   string
	quoted bool // the whole token was in quotes: a text search
	not    bool // -"text": a negated text search
}

func (t token) display() string {
	if t.quoted {
		prefix := ""
		if t.not {
			prefix = "-"
		}
		return prefix + strconv.Quote(t.text)
	}
	return t.text
}

// tokenize splits on spaces outside double quotes. A token that is a quoted string as a whole
// ("timeout", or -"timeout") is a text search; quotes inside a token (data.note="a b") only keep
// its spaces.
func tokenize(text string) ([]token, error) {
	var out []token
	var current strings.Builder
	var tok token
	inQuote, started := false, false
	flush := func() {
		if started {
			tok.text = current.String()
			out = append(out, tok)
		}
		current.Reset()
		tok, inQuote, started = token{}, false, false
	}
	for _, r := range text {
		switch {
		case r == '"':
			if !started || (!tok.quoted && !inQuote && current.String() == "-") {
				tok.quoted, tok.not = true, started
				current.Reset()
			}
			started = true
			inQuote = !inQuote
		case !inQuote && (r == ' ' || r == '\t' || r == '\n'):
			flush()
		default:
			if !inQuote && tok.quoted {
				// Text after the closing quote: not a quoted string as a whole.
				tok.quoted = false
			}
			started = true
			current.WriteRune(r)
		}
	}
	if inQuote {
		return nil, errors.New("unclosed quote")
	}
	flush()
	return out, nil
}

func (f *Filter) parseToken(tok token) (Term, bool, error) {
	text := tok.text
	not := false
	if !tok.quoted && strings.HasPrefix(text, "-") && len(text) > 1 {
		not, text = true, text[1:]
	}
	if tok.quoted {
		if strings.TrimSpace(text) == "" {
			return Term{}, false, errors.New("empty text search")
		}
		return Term{Kind: TextTerm, Not: tok.not, Values: []string{text}}, false, nil
	}
	if label, ok := strings.CutPrefix(text, "#"); ok {
		values, err := alternatives(label)
		return Term{Kind: LabelTerm, Not: not, Values: values}, false, err
	}
	opAt, op := firstOperator(text)
	colonAt := strings.IndexByte(text, ':')
	if opAt >= 0 && (colonAt < 0 || opAt < colonAt) {
		name, value := text[:opAt], text[opAt+len(op):]
		switch name {
		case "since", "from", "to":
			if not {
				return Term{}, false, errors.New("a time range cannot be negated")
			}
			return Term{}, true, f.parseTime(name, op, value)
		}
		term, err := parseComparison(name, op, value)
		term.Not = not
		return term, false, err
	}
	if colonAt >= 0 {
		key, value := text[:colonAt], text[colonAt+1:]
		if key == "" {
			return Term{}, false, errors.New("a tag needs a key before the colon")
		}
		if value == "" {
			return Term{Kind: TagKeyTerm, Not: not, Field: key}, false, nil
		}
		values, err := alternatives(value)
		return Term{Kind: TagTerm, Not: not, Field: key, Values: values}, false, err
	}
	if prefix, ok := strings.CutSuffix(text, "*"); ok {
		if prefix == "" || strings.ContainsAny(prefix, "*,") {
			return Term{}, false, errors.New("an event prefix is one word followed by *")
		}
		return Term{Kind: EventPrefixTerm, Not: not, Values: []string{prefix}}, false, nil
	}
	values, err := alternatives(text)
	return Term{Kind: EventTerm, Not: not, Values: values}, false, err
}

func firstOperator(text string) (int, string) {
	at, found := -1, ""
	for _, op := range operators {
		if index := strings.Index(text, op); index >= 0 && (at < 0 || index < at || (index == at && len(op) > len(found))) {
			at, found = index, op
		}
	}
	return at, found
}

func parseComparison(name, op, value string) (Term, error) {
	switch {
	case name == "value":
		number, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return Term{}, errors.New("value compares with a number")
		}
		return Term{Kind: ValueTerm, Op: op, Number: number}, nil
	case strings.HasPrefix(name, "data."):
		path := strings.TrimPrefix(name, "data.")
		if !dataPathPattern.MatchString(path) {
			return Term{}, errors.New("a data path is letters, digits and _ separated by dots")
		}
		if op == "=" {
			values, err := alternatives(value)
			return Term{Kind: DataTerm, Field: path, Op: op, Values: values}, err
		}
		number, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return Term{}, fmt.Errorf("%s compares with a number", op)
		}
		return Term{Kind: DataTerm, Field: path, Op: op, Number: number}, nil
	}
	column, ok := fieldColumns[name]
	if !ok {
		return Term{}, fmt.Errorf("unknown field %q (fields: ns, user, anon, tenant, country, req, value, data.<key>)", name)
	}
	if op != "=" {
		return Term{}, fmt.Errorf("%s only compares with =", name)
	}
	values, err := alternatives(value)
	return Term{Kind: FieldTerm, Field: column, Values: values}, err
}

func alternatives(text string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(text, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("missing value")
	}
	return out, nil
}

func (f *Filter) parseTime(name, op, value string) error {
	if op != "=" {
		return fmt.Errorf("%s takes =", name)
	}
	if name == "since" {
		duration, err := ParseDuration(value)
		if err != nil || duration <= 0 {
			return errors.New("since takes a duration like 30m, 24h or 7d")
		}
		f.Since = duration
		return nil
	}
	at, dateOnly, err := parseMoment(value)
	if err != nil {
		return err
	}
	if name == "from" {
		f.From = at
	} else {
		if dateOnly {
			at = at.AddDate(0, 0, 1)
		}
		f.To = at
	}
	return nil
}

// ParseDuration reads Go durations plus a d (day) unit: 7d, 1d12h.
func ParseDuration(text string) (time.Duration, error) {
	days := time.Duration(0)
	if index := strings.IndexByte(text, 'd'); index > 0 {
		count, err := strconv.Atoi(text[:index])
		if err != nil {
			return 0, err
		}
		days = time.Duration(count) * 24 * time.Hour
		text = text[index+1:]
		if text == "" {
			return days, nil
		}
	}
	rest, err := time.ParseDuration(text)
	return days + rest, err
}

func parseMoment(value string) (time.Time, bool, error) {
	if at, err := time.Parse(DateLayout, value); err == nil {
		return at, true, nil
	}
	for _, layout := range []string{"2006-01-02T15:04", time.RFC3339} {
		if at, err := time.Parse(layout, value); err == nil {
			return at.UTC(), false, nil
		}
	}
	return time.Time{}, false, errors.New("a time is YYYY-MM-DD, YYYY-MM-DDTHH:MM or RFC3339 (UTC)")
}

// Range is the time window of the filter at now. A zero bound is open.
func (f Filter) Range(now time.Time) (time.Time, time.Time) {
	from, to := f.From, f.To
	if f.Since > 0 {
		since := now.Add(-f.Since)
		if from.IsZero() || since.After(from) {
			from = since
		}
	}
	return from, to
}

// only is the filter reduced to the terms keep accepts, without a time range.
func (f Filter) only(keep func(Term) bool) Filter {
	var out Filter
	for _, term := range f.Terms {
		if keep(term) {
			out.Terms = append(out.Terms, term)
		}
	}
	return out
}

// Events returns the event names the filter requires, if its positive event term is an exact
// list; nil means any event.
func (f Filter) Events() []string {
	for _, term := range f.Terms {
		if term.Kind == EventTerm && !term.Not {
			return term.Values
		}
	}
	return nil
}

// IndexOnly reports whether the filter uses nothing the indexes cannot answer: event names,
// namespaces and the time range. Such a filter is served from _daily and _facets without reading
// events.
func (f Filter) IndexOnly() bool {
	for _, term := range f.Terms {
		switch {
		case term.Kind == EventTerm, term.Kind == EventPrefixTerm:
		case term.Kind == FieldTerm && term.Field == "ns":
		default:
			return false
		}
	}
	return true
}

// Where is the filter as a DuckDB boolean expression over the events view. since= stays relative
// (now() at query time), so a saved view written to views.sql keeps meaning "the last 7 days".
// Every literal is escaped here, so a filter can never carry SQL of its own.
func (f Filter) Where() string {
	var parts []string
	if f.Since > 0 {
		interval := fmt.Sprintf("to_seconds(%d)", int64(f.Since.Seconds()))
		// The date bound only prunes partitions; a day of slack keeps it right in any TimeZone.
		parts = append(parts, fmt.Sprintf("ts >= now() - %s AND date >= CAST(now() - %s AS DATE) - 1", interval, interval))
	}
	if !f.From.IsZero() {
		parts = append(parts, fmt.Sprintf("ts >= %s AND date >= DATE %s", timestampSQL(f.From), quote(f.From.UTC().Format(DateLayout))))
	}
	if !f.To.IsZero() {
		parts = append(parts, fmt.Sprintf("ts < %s AND date <= DATE %s", timestampSQL(f.To), quote(f.To.UTC().Format(DateLayout))))
	}
	for _, term := range f.Terms {
		expression := term.sql()
		if term.Not {
			expression = "NOT coalesce(" + expression + ", false)"
		}
		parts = append(parts, expression)
	}
	if len(parts) == 0 {
		return "true"
	}
	return strings.Join(parts, " AND ")
}

func (t Term) sql() string {
	switch t.Kind {
	case EventTerm:
		return inSQL("event", t.Values)
	case EventPrefixTerm:
		return fmt.Sprintf("starts_with(event, %s)", quote(t.Values[0]))
	case FieldTerm:
		return inSQL(t.Field, t.Values)
	case ValueTerm:
		return fmt.Sprintf("value %s %s", t.Op, number(t.Number))
	case TagTerm:
		tags := make([]string, len(t.Values))
		for i, value := range t.Values {
			tags[i] = t.Field + ":" + value
		}
		return fmt.Sprintf("list_has_any(tags, %s)", listSQL(tags))
	case TagKeyTerm:
		return fmt.Sprintf("len(list_filter(tags, lambda t: starts_with(t, %s))) > 0", quote(t.Field+":"))
	case LabelTerm:
		return fmt.Sprintf("list_has_any(tags, %s)", listSQL(t.Values))
	case DataTerm:
		path := quote("$." + t.Field)
		if t.Op == "=" {
			return inSQL(fmt.Sprintf("json_extract_string(data, %s)", path), t.Values)
		}
		return fmt.Sprintf("TRY_CAST(json_extract_string(data, %s) AS DOUBLE) %s %s", path, t.Op, number(t.Number))
	case TextTerm:
		return fmt.Sprintf("contains(lower(msg), %s)", quote(strings.ToLower(t.Values[0])))
	}
	return "false"
}

func inSQL(column string, values []string) string {
	if len(values) == 1 {
		return fmt.Sprintf("%s = %s", column, quote(values[0]))
	}
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = quote(value)
	}
	return fmt.Sprintf("%s IN (%s)", column, strings.Join(quoted, ", "))
}

func listSQL(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = quote(value)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// quote is a DuckDB string literal. DuckDB strings only escape the quote itself.
func quote(text string) string { return "'" + strings.ReplaceAll(text, "'", "''") + "'" }

func number(value float64) string { return strconv.FormatFloat(value, 'g', -1, 64) }

func timestampSQL(at time.Time) string {
	return "TIMESTAMPTZ " + quote(at.UTC().Format("2006-01-02 15:04:05.000")+"+00")
}

// Match is the filter as a Go predicate, for today's events and for readers without DuckDB. ns
// and date come from the partition the row was read from.
func (f Filter) Match(row Row, ns string, now time.Time) bool {
	from, to := f.Range(now)
	if !from.IsZero() && row.TS.Before(from) {
		return false
	}
	if !to.IsZero() && !row.TS.Before(to) {
		return false
	}
	var data map[string]any
	for _, term := range f.Terms {
		if term.Kind == DataTerm && data == nil && row.Data != "" {
			decoder := json.NewDecoder(strings.NewReader(row.Data))
			decoder.UseNumber()
			decoder.Decode(&data)
		}
		if term.match(row, ns, data) == term.Not {
			return false
		}
	}
	return true
}

func (t Term) match(row Row, ns string, data map[string]any) bool {
	switch t.Kind {
	case EventTerm:
		return slices.Contains(t.Values, row.Event)
	case EventPrefixTerm:
		return strings.HasPrefix(row.Event, t.Values[0])
	case FieldTerm:
		var value string
		switch t.Field {
		case "ns":
			value = ns
		case "user_id":
			value = row.UserID
		case "anon_id":
			value = row.AnonID
		case "tenant_id":
			value = row.TenantID
		case "country":
			value = row.Country
		case "request_id":
			value = row.RequestID
		}
		return value != "" && slices.Contains(t.Values, value)
	case ValueTerm:
		return row.Value != nil && compare(*row.Value, t.Op, t.Number)
	case TagTerm:
		for _, value := range t.Values {
			if slices.Contains(row.Tags, t.Field+":"+value) {
				return true
			}
		}
		return false
	case TagKeyTerm:
		return slices.ContainsFunc(row.Tags, func(tag string) bool { return strings.HasPrefix(tag, t.Field+":") })
	case LabelTerm:
		return slices.ContainsFunc(t.Values, func(value string) bool { return slices.Contains(row.Tags, value) })
	case DataTerm:
		value, ok := lookup(data, t.Field)
		if !ok || value == nil {
			return false
		}
		text := scalarText(value)
		if t.Op == "=" {
			return text != "" && slices.Contains(t.Values, text)
		}
		parsed, err := strconv.ParseFloat(text, 64)
		return err == nil && compare(parsed, t.Op, t.Number)
	case TextTerm:
		return strings.Contains(strings.ToLower(row.Msg), strings.ToLower(t.Values[0]))
	}
	return false
}

func lookup(data map[string]any, path string) (any, bool) {
	var current any = data
	for part := range strings.SplitSeq(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		if current, ok = object[part]; !ok {
			return nil, false
		}
	}
	return current, true
}

// scalarText renders a JSON value the way json_extract_string does: strings bare, numbers and
// booleans as written, objects and arrays as JSON.
func scalarText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	default:
		encoded, _ := json.Marshal(v)
		return string(encoded)
	}
}

func compare(left float64, op string, right float64) bool {
	switch op {
	case "=":
		return left == right
	case ">":
		return left > right
	case ">=":
		return left >= right
	case "<":
		return left < right
	case "<=":
		return left <= right
	}
	return false
}
