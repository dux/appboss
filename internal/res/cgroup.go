package res

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultCgroupRoot is the parent cgroup appboss creates one directory under per process. It is
// on the cgroup v2 unified hierarchy; a box without it falls back to the procgroup backend.
const DefaultCgroupRoot = "/sys/fs/cgroup/boss"

type cpuSample struct {
	usage int64
	at    time.Time
}

// Cgroup is the Linux cgroup v2 backend. It creates one cgroup per (app, process), applies the
// memory and CPU limits there and reads the process's use back. Writes happen through the normal
// filesystem interface, so the type compiles and is unit-testable on any platform.
type Cgroup struct {
	root string
	mu   sync.Mutex
	cpu  map[string]cpuSample
}

func NewCgroup(root string) *Cgroup { return &Cgroup{root: root, cpu: map[string]cpuSample{}} }

func (*Cgroup) Name() string { return "cgroup" }

// Available reports whether the cgroup hierarchy exists and appboss can write to it. It enables
// the memory and cpu controllers for child cgroups, which a delegated parent must allow.
func Available(root string) bool {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return false
	}
	_ = os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+memory +cpu"), 0o644)
	probe := filepath.Join(root, ".appboss-write-test")
	if err := os.WriteFile(probe, []byte("1"), 0o644); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

func (c *Cgroup) dir(app, proc string) string { return filepath.Join(c.root, app, proc) }

func (c *Cgroup) Place(app, proc string, pid int, limits Limits) error {
	dir := c.dir(app, proc)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if limits.MemoryMax > 0 {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatInt(limits.MemoryMax, 10)), 0o644); err != nil {
			return err
		}
	}
	if limits.CPUMax > 0 {
		// cpu.max is "<quota> <period>" in microseconds; 100000us per 100ms is one core, so a
		// percentage becomes a quota of percent*1000.
		quota := strconv.Itoa(limits.CPUMax*1000) + " 100000"
		if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(quota), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644)
}

func (c *Cgroup) Stats(app, proc string, _ []int) (Stats, error) {
	dir := c.dir(app, proc)
	var stats Stats
	if data, err := os.ReadFile(filepath.Join(dir, "memory.current")); err == nil {
		stats.MemoryBytes, _ = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "cpu.stat")); err == nil {
		stats.CPUPercent = c.cpuPercent(app, proc, cpuUsageUsec(string(data)), time.Now())
	}
	return stats, nil
}

// cpuPercent turns the cumulative usage counter into a percentage of one core since the last
// sample. The first call has no previous sample and reports zero.
func (c *Cgroup) cpuPercent(app, proc string, usage int64, now time.Time) float64 {
	key := app + "/" + proc
	c.mu.Lock()
	previous, ok := c.cpu[key]
	c.cpu[key] = cpuSample{usage: usage, at: now}
	c.mu.Unlock()
	if !ok {
		return 0
	}
	if elapsed := now.Sub(previous.at).Seconds(); elapsed > 0 {
		return float64(usage-previous.usage) / 1e6 / elapsed * 100
	}
	return 0
}

// Release removes the process's cgroup directory once the process is gone. A directory that is
// still busy is left for the next spawn to reuse.
func (c *Cgroup) Release(app, proc string) error {
	_ = os.Remove(c.dir(app, proc))
	return nil
}

// cpuUsageUsec reads the usage_usec line of a cgroup v2 cpu.stat file.
func cpuUsageUsec(contents string) int64 {
	for _, line := range strings.Split(contents, "\n") {
		if value, ok := strings.CutPrefix(line, "usage_usec "); ok {
			usage, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			return usage
		}
	}
	return 0
}
