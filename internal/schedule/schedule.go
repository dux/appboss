// Package schedule parses the two schedule formats a cron job accepts: an "every <interval>"
// shorthand and a standard five-field cron expression. It has no dependencies so the config
// loader can validate it and the supervisor can ask for the next run time.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed schedule: either an interval (every 5m) or five cron fields.
type Schedule struct {
	raw      string
	valid    bool
	interval time.Duration
	fields   [5]field
	domAny   bool
	dowAny   bool
}

type field struct {
	any    bool
	values map[int]bool
}

func (f field) has(value int) bool { return f.any || f.values[value] }

var fieldRanges = [5][2]int{
	{0, 59}, // minute
	{0, 23}, // hour
	{1, 31}, // day of month
	{1, 12}, // month
	{0, 7},  // day of week, 0 and 7 are Sunday
}

var fieldNames = [5]string{"minute", "hour", "day", "month", "weekday"}

var monthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var weekdayNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

// Parse reads an "every <n><s|m|h|d>" interval or a five-field cron expression.
func Parse(spec string) (Schedule, error) {
	raw := strings.TrimSpace(spec)
	if raw == "" {
		return Schedule{}, fmt.Errorf("schedule is empty")
	}
	if rest, ok := strings.CutPrefix(strings.ToLower(raw), "every "); ok {
		interval, err := parseInterval(strings.TrimSpace(rest))
		if err != nil {
			return Schedule{}, err
		}
		return Schedule{raw: raw, valid: true, interval: interval}, nil
	}
	parts := strings.Fields(raw)
	if len(parts) != 5 {
		return Schedule{}, fmt.Errorf("cron expression must have 5 fields (minute hour day month weekday), got %d", len(parts))
	}
	parsed := Schedule{raw: raw, valid: true}
	for i, part := range parts {
		names := map[string]int(nil)
		switch i {
		case 3:
			names = monthNames
		case 4:
			names = weekdayNames
		}
		value, err := parseField(part, fieldRanges[i][0], fieldRanges[i][1], names)
		if err != nil {
			return Schedule{}, fmt.Errorf("cron %s field %q: %w", fieldNames[i], part, err)
		}
		parsed.fields[i] = value
	}
	// Sunday is both 0 and 7; collapse 7 onto 0 so Weekday() matches.
	if parsed.fields[4].values[7] {
		parsed.fields[4].values[0] = true
		delete(parsed.fields[4].values, 7)
	}
	parsed.domAny = parsed.fields[2].any
	parsed.dowAny = parsed.fields[4].any
	return parsed, nil
}

func parseInterval(value string) (time.Duration, error) {
	digits := 0
	for digits < len(value) && value[digits] >= '0' && value[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, intervalError(value)
	}
	count, err := strconv.Atoi(value[:digits])
	if err != nil || count <= 0 {
		return 0, intervalError(value)
	}
	var scale time.Duration
	switch strings.TrimSpace(value[digits:]) {
	case "s", "sec", "secs", "second", "seconds":
		scale = time.Second
	case "m", "min", "mins", "minute", "minutes":
		scale = time.Minute
	case "h", "hr", "hrs", "hour", "hours":
		scale = time.Hour
	case "d", "day", "days":
		scale = 24 * time.Hour
	default:
		return 0, intervalError(value)
	}
	return time.Duration(count) * scale, nil
}

func intervalError(value string) error {
	return fmt.Errorf("invalid interval %q (use every 5m, every 2h or every 1d)", value)
}

func parseField(spec string, low, high int, names map[string]int) (field, error) {
	if spec == "*" {
		return field{any: true}, nil
	}
	values := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		if part == "" {
			return field{}, fmt.Errorf("empty list entry")
		}
		step := 1
		base := part
		if slash := strings.IndexByte(part, '/'); slash >= 0 {
			base = part[:slash]
			parsed, err := strconv.Atoi(part[slash+1:])
			if err != nil || parsed < 1 {
				return field{}, fmt.Errorf("invalid step %q", part[slash+1:])
			}
			step = parsed
		}
		start, end := low, high
		switch {
		case base == "*":
		case strings.ContainsRune(base, '-'):
			first, second, _ := strings.Cut(base, "-")
			var err error
			if start, err = fieldValue(first, names); err != nil {
				return field{}, err
			}
			if end, err = fieldValue(second, names); err != nil {
				return field{}, err
			}
			if start > end {
				return field{}, fmt.Errorf("range %q is reversed", base)
			}
		default:
			value, err := fieldValue(base, names)
			if err != nil {
				return field{}, err
			}
			start, end = value, value
			// "5/10" means every 10 from 5 to the end of the range.
			if step > 1 {
				end = high
			}
		}
		if start < low || end > high {
			return field{}, fmt.Errorf("%q is outside %d-%d", base, low, high)
		}
		for value := start; value <= end; value += step {
			values[value] = true
		}
	}
	if len(values) == 0 {
		return field{}, fmt.Errorf("matches nothing")
	}
	return field{values: values}, nil
}

func fieldValue(token string, names map[string]int) (int, error) {
	token = strings.TrimSpace(token)
	if names != nil {
		if value, ok := names[strings.ToUpper(token)]; ok {
			return value, nil
		}
	}
	value, err := strconv.Atoi(token)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", token)
	}
	return value, nil
}

// Next returns the first run time strictly after after. A zero time means the schedule could not
// be satisfied within five years. Interval schedules simply add their interval.
func (s Schedule) Next(after time.Time) time.Time {
	if !s.valid {
		return time.Time{}
	}
	if s.interval > 0 {
		return after.Add(s.interval)
	}
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.AddDate(5, 0, 0)
	for t.Before(limit) {
		if !s.fields[3].has(int(t.Month())) || !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
			continue
		}
		if !s.fields[1].has(t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location()).Add(time.Hour)
			continue
		}
		if !s.fields[0].has(t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// dayMatches follows Vixie cron: with both day fields restricted a run matches either of them.
func (s Schedule) dayMatches(t time.Time) bool {
	day := s.fields[2].has(t.Day())
	weekday := s.fields[4].has(int(t.Weekday()))
	switch {
	case s.domAny && s.dowAny:
		return true
	case s.domAny:
		return weekday
	case s.dowAny:
		return day
	default:
		return day || weekday
	}
}

// IsZero reports whether the schedule was never parsed.
func (s Schedule) IsZero() bool { return !s.valid }

// String returns the schedule as written.
func (s Schedule) String() string { return s.raw }
