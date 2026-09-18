package daemon

import (
	"testing"

	"deploy-boss/internal/config"
)

func TestManagementTakesFirstPort(t *testing.T) {
	cfg := config.Default()
	cfg.Ports.Range = [2]int{3100, 3199}
	allocator, port := newAllocator(cfg)
	if port != 3100 || allocator.Entries()["dboss/management"] != 3100 {
		t.Fatalf("management port = %d, entries = %v", port, allocator.Entries())
	}
	if next, _ := allocator.Allocate("alpha", "web"); next != 3101 {
		t.Fatalf("first app port = %d, want 3101", next)
	}
}
