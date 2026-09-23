package res

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestCgroupPlaceStatsRelease(t *testing.T) {
	root := t.TempDir()
	cgroup := NewCgroup(root)
	if err := cgroup.Place("demo", "web", 4321, Limits{MemoryMax: 512 << 20, CPUMax: 150}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "demo", "web")
	for file, want := range map[string]string{
		"cgroup.procs": "4321",
		"memory.max":   strconv.Itoa(512 << 20),
		"cpu.max":      "150000 100000",
	} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v; want %q", file, data, err, want)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("123456\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("usage_usec 1000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := cgroup.Stats("demo", "web", []int{4321})
	if err != nil || first.MemoryBytes != 123456 || first.CPUPercent != 0 || first.Approximate {
		t.Fatalf("first sample = %+v, %v", first, err)
	}

	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("usage_usec 3000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := cgroup.Stats("demo", "web", nil)
	if err != nil || second.CPUPercent <= 0 {
		t.Fatalf("second sample should report CPU use: %+v, %v", second, err)
	}

	// A real cgroup directory holds no regular files (the kernel drops its pseudo-files with the
	// directory); clear the ones this test wrote so Release can rmdir it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if err := cgroup.Release("demo", "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cgroup directory should be removed: %v", err)
	}
}

func TestCgroupAvailableAndUsageParse(t *testing.T) {
	if !Available(t.TempDir()) {
		t.Fatal("a writable directory should be available")
	}
	if got := cpuUsageUsec("nr_periods 1\nusage_usec 42\n"); got != 42 {
		t.Fatalf("usage = %d, want 42", got)
	}
	if got := cpuUsageUsec("nr_periods 1\n"); got != 0 {
		t.Fatalf("missing usage = %d, want 0", got)
	}
}

func TestCgroupOOMKills(t *testing.T) {
	root := t.TempDir()
	cgroup := NewCgroup(root)
	if got := cgroup.OOMKills("demo", "web"); got != 0 {
		t.Fatalf("missing memory.events = %d", got)
	}
	dir := filepath.Join(root, "demo", "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	events := "low 0\nhigh 0\nmax 4\noom 2\noom_kill 2\noom_group_kill 0\n"
	if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := cgroup.OOMKills("demo", "web"); got != 2 {
		t.Fatalf("OOMKills = %d, want 2", got)
	}
}
