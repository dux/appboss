package schedule

import (
	"testing"
	"time"
)

func TestParseIntervals(t *testing.T) {
	for _, test := range []struct {
		spec string
		want time.Duration
	}{
		{"every 30s", 30 * time.Second},
		{"every 5m", 5 * time.Minute},
		{"every 2h", 2 * time.Hour},
		{"every 1d", 24 * time.Hour},
		{"every 3 days", 72 * time.Hour},
		{"Every 10 minutes", 10 * time.Minute},
		{"every 1 hr", time.Hour},
	} {
		schedule, err := Parse(test.spec)
		if err != nil {
			t.Fatalf("Parse(%q): %v", test.spec, err)
		}
		if schedule.IsZero() {
			t.Fatalf("Parse(%q) returned a zero schedule", test.spec)
		}
		base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		if got := schedule.Next(base); !got.Equal(base.Add(test.want)) {
			t.Errorf("Parse(%q).Next = %s, want %s", test.spec, got, base.Add(test.want))
		}
	}
}

func TestParseRejectsBadIntervals(t *testing.T) {
	for _, spec := range []string{"every", "every x", "every 0m", "every 5x", "every -1m"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) accepted an invalid interval", spec)
		}
	}
}

func TestParseRejectsBadCron(t *testing.T) {
	for _, spec := range []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "*/0 * * * *", "5-1 * * * *", "MON * * * *"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) accepted an invalid cron expression", spec)
		}
	}
}

func TestCronNext(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC) // Thursday
	for _, test := range []struct {
		spec string
		want time.Time
	}{
		{"* * * * *", time.Date(2026, 1, 1, 10, 31, 0, 0, time.UTC)},
		{"*/15 * * * *", time.Date(2026, 1, 1, 10, 45, 0, 0, time.UTC)},
		{"0 * * * *", time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)},
		{"0 3 * * *", time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)},
		{"0 7 * * 1-5", time.Date(2026, 1, 2, 7, 0, 0, 0, time.UTC)},
		{"30 10 * FEB *", time.Date(2026, 2, 1, 10, 30, 0, 0, time.UTC)},
		{"0 0 1 * *", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{"0 0 * * SUN", time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)},
		{"0 0 13 * FRI", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
	} {
		schedule, err := Parse(test.spec)
		if err != nil {
			t.Fatalf("Parse(%q): %v", test.spec, err)
		}
		if got := schedule.Next(base); !got.Equal(test.want) {
			t.Errorf("Parse(%q).Next(%s) = %s, want %s", test.spec, base, got, test.want)
		}
	}
}

func TestCronSundaySeven(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC) // Thursday
	schedule, err := Parse("0 0 * * 7")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
	if got := schedule.Next(base); !got.Equal(want) {
		t.Errorf("Next = %s, want %s", got, want)
	}
}
