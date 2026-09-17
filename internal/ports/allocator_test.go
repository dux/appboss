package ports

import "testing"

func TestSequentialStickyAllocation(t *testing.T) {
	a := New([2]int{32001, 32002})
	first, err := a.Allocate("app", "web")
	if err != nil {
		t.Fatal(err)
	}
	if first != 32001 {
		t.Fatalf("first port = %d, want 32001", first)
	}
	again, _ := a.Allocate("app", "web")
	if first != again {
		t.Fatalf("allocation was not sticky: %d != %d", first, again)
	}
	second, err := a.Allocate("app", "worker")
	if err != nil || second != 32002 {
		t.Fatalf("second port = %d, %v; want 32002", second, err)
	}
	if _, err := a.Allocate("other", "web"); err == nil {
		t.Fatal("expected exhausted range")
	}
	if port, ok := a.Lookup("app", "worker"); !ok || port != 32002 {
		t.Fatalf("lookup = %d, %v", port, ok)
	}
}
