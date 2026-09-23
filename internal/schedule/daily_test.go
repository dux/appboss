package schedule

import (
	"testing"
	"time"
)

func TestNextDaily(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if got := NextDaily("04:00", now); !got.Equal(time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("NextDaily(past) = %s, want tomorrow 04:00", got)
	}
	if got := NextDaily("18:00", now); !got.Equal(time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("NextDaily(future) = %s, want today 18:00", got)
	}
	if got := NextDaily("12:00", now); !got.Equal(time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("NextDaily(now) = %s, want tomorrow 12:00", got)
	}
	for _, at := range []string{"", "25:00", "noon"} {
		if got := NextDaily(at, now); !got.IsZero() {
			t.Fatalf("NextDaily(%q) = %s, want zero", at, got)
		}
	}
}
