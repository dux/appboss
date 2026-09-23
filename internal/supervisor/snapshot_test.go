package supervisor

import "testing"

func TestSnapshotServing(t *testing.T) {
	cases := []struct {
		name     string
		snapshot Snapshot
		want     bool
	}{
		{"running", Snapshot{State: Running}, true},
		{"stopped wakes on request", Snapshot{State: Stopped}, true},
		{"stopped button app", Snapshot{State: Stopped, WakeButton: true}, false},
		{"running button app", Snapshot{State: Running, WakeButton: true}, true},
		{"starting", Snapshot{State: Starting}, false},
		{"stopping", Snapshot{State: Stopping}, false},
		{"crashed", Snapshot{State: Crashed}, false},
		{"maintenance", Snapshot{State: Running, Maintenance: true}, false},
		{"draining", Snapshot{State: Running, Draining: true}, false},
	}
	for _, c := range cases {
		if got := c.snapshot.Serving(); got != c.want {
			t.Errorf("%s: Serving() = %v, want %v", c.name, got, c.want)
		}
	}
}
