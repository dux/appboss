package cli

import "testing"

func TestLogOverlapTracksRollingTail(t *testing.T) {
	previous := []string{"a", "b", "c"}
	current := []string{"b", "c", "d"}
	if got := logOverlap(previous, current); got != 2 {
		t.Fatalf("got overlap %d", got)
	}
	if got := logOverlap(previous, []string{"x"}); got != 0 {
		t.Fatalf("got unrelated overlap %d", got)
	}
}
