package ports

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"dboss/internal/fsutil"
)

// freeWindow finds a few consecutive ports nothing listens on, so the tests do not depend on
// what else runs on the machine.
func freeWindow(t *testing.T, size int) [2]int {
	t.Helper()
	for first := 42000; first < 43000; first += size {
		ok := true
		for port := first; port < first+size; port++ {
			if !Free(port) {
				ok = false
				break
			}
		}
		if ok {
			return [2]int{first, first + size - 1}
		}
	}
	t.Skip("no free port window")
	return [2]int{}
}

func openRegistry(t *testing.T, dir string, window [2]int) *Registry {
	t.Helper()
	registry, err := OpenRegistry(dir, filepath.Join(t.TempDir(), "ports.json"), window)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return registry
}

func TestRegistrySessionsNeverShareAPort(t *testing.T) {
	dir, window := t.TempDir(), freeWindow(t, 4)
	first, second := openRegistry(t, dir, window), openRegistry(t, dir, window)
	a, err := first.Claim("app/web")
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Claim("app/web")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("both sessions got port %d", a)
	}
	if again, _ := first.Claim("app/web"); again != a {
		t.Fatalf("claim was not sticky: %d then %d", a, again)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third := openRegistry(t, dir, window)
	if c, err := third.Claim("app/web"); err != nil || c != a {
		t.Fatalf("closed session's port not reused: %d, %v; want %d", c, err, a)
	}
}

func TestRegistryDropsDeadSessions(t *testing.T) {
	dir, window := t.TempDir(), freeWindow(t, 2)
	// pid 0 never names a live process.
	dead := session{PID: 0, Ports: map[string]int{"other/web": window[0]}}
	if err := fsutil.WriteJSON(filepath.Join(dir, "1-dead.json"), dead, 0o600); err != nil {
		t.Fatal(err)
	}
	port, err := openRegistry(t, dir, window).Claim("app/web")
	if err != nil || port != window[0] {
		t.Fatalf("port = %d, %v; want %d", port, err, window[0])
	}
	if _, err := os.Stat(filepath.Join(dir, "1-dead.json")); !os.IsNotExist(err) {
		t.Fatalf("dead session file kept: %v", err)
	}
}

func TestRegistrySkipsBusyPorts(t *testing.T) {
	window := freeWindow(t, 2)
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(window[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port, err := openRegistry(t, t.TempDir(), window).Claim("app/web")
	if err != nil || port != window[1] {
		t.Fatalf("port = %d, %v; want %d", port, err, window[1])
	}
}

func TestRegistryPrefersPreviousPortAndReportsStale(t *testing.T) {
	dir, window := t.TempDir(), freeWindow(t, 3)
	state := filepath.Join(t.TempDir(), "ports.json")
	if err := fsutil.WriteJSON(state, map[string]int{"app/web": window[1]}, 0o640); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(dir, state, window)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	stale, err := registry.Stale()
	if err != nil || len(stale) != 1 || stale[0] != window[1] {
		t.Fatalf("stale = %v, %v", stale, err)
	}
	if port, err := registry.Claim("app/web"); err != nil || port != window[1] {
		t.Fatalf("port = %d, %v; want previous %d", port, err, window[1])
	}
	other := openRegistry(t, dir, window)
	if _, err := other.Claim("x/web"); err != nil {
		t.Fatal(err)
	}
	// A port a live session holds is never stale, even when it was this folder's last time.
	other.previous = map[string]int{"app/web": window[1]}
	if stale, _ := other.Stale(); len(stale) != 0 {
		t.Fatalf("held port reported stale: %v", stale)
	}
}

func TestRegistryExhausted(t *testing.T) {
	window := freeWindow(t, 1)
	registry := openRegistry(t, t.TempDir(), window)
	if _, err := registry.Claim("a/web"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Claim("b/web"); err == nil {
		t.Fatal("expected no free port")
	}
}

func TestClaimingAllocator(t *testing.T) {
	window := freeWindow(t, 2)
	allocator := NewClaiming(openRegistry(t, t.TempDir(), window).Claim)
	first, err := allocator.Allocate("app", "web")
	if err != nil || first != window[0] {
		t.Fatalf("first = %d, %v", first, err)
	}
	if port, ok := allocator.Lookup("app", "web"); !ok || port != first {
		t.Fatalf("lookup = %d, %v", port, ok)
	}
}
