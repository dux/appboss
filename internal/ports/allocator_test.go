package ports

import "testing"

func TestStickyAllocationAndRelease(t *testing.T) {
	a, err := Open(t.TempDir(), [2]int{32001, 32003}, false)
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.Allocate("app", "web")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := a.Allocate("app", "web")
	if first != again {
		t.Fatalf("allocation was not sticky: %d != %d", first, again)
	}
	if err := a.Release("app", false); err == nil {
		t.Fatal("expected running-app rejection")
	}
	if err := a.Release("app", true); err != nil {
		t.Fatal(err)
	}
}
