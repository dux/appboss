package res

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

type Stats struct {
	MemoryBytes int64   `json:"memory_bytes"`
	CPUPercent  float64 `json:"cpu_percent"`
	Approximate bool    `json:"approximate"`
}

// Limits are the per-process resource limits from config. They are only enforced by the cgroup
// backend; the procgroup backend ignores them.
type Limits struct {
	MemoryMax int64
	CPUMax    int
}

// Backend places a process in a resource domain, reports its use and releases the domain when the
// process is gone. app is the app name, proc the procfile process name.
type Backend interface {
	Name() string
	Place(app, proc string, pid int, limits Limits) error
	Stats(app, proc string, pids []int) (Stats, error)
	Release(app, proc string) error
	// OOMKills is the cumulative count of kernel OOM kills in the process's domain; a backend that
	// cannot tell returns 0.
	OOMKills(app, proc string) int64
}

type Procgroup struct{}

func (Procgroup) Name() string { return "procgroup" }

func (Procgroup) Place(string, string, int, Limits) error { return nil }

func (Procgroup) Release(string, string) error { return nil }

func (Procgroup) OOMKills(string, string) int64 { return 0 }

func (Procgroup) Stats(_ string, _ string, pids []int) (Stats, error) {
	stats := Stats{Approximate: true}
	for _, pid := range pids {
		output, err := exec.Command("ps", "-o", "rss=,%cpu=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			continue
		}
		fields := strings.Fields(string(output))
		if len(fields) != 2 {
			continue
		}
		rss, _ := strconv.ParseInt(fields[0], 10, 64)
		cpu, _ := strconv.ParseFloat(strings.ReplaceAll(fields[1], ",", "."), 64)
		stats.MemoryBytes += rss * 1024
		stats.CPUPercent += cpu
	}
	return stats, nil
}

func Signal(name string) (syscall.Signal, error) {
	switch name {
	case "TERM":
		return syscall.SIGTERM, nil
	case "INT":
		return syscall.SIGINT, nil
	case "QUIT":
		return syscall.SIGQUIT, nil
	case "USR1":
		return syscall.SIGUSR1, nil
	case "USR2":
		return syscall.SIGUSR2, nil
	default:
		return 0, fmt.Errorf("unsupported signal %q", name)
	}
}
