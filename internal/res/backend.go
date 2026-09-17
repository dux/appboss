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

type Backend interface {
	Place(pid int) error
	KillAll(pids []int, signal syscall.Signal) error
	Stats(pids []int) (Stats, error)
}

type Procgroup struct{}

func (Procgroup) Place(int) error { return nil }

func (Procgroup) KillAll(pids []int, signal syscall.Signal) error {
	var first error
	for _, pid := range pids {
		if err := syscall.Kill(-pid, signal); err != nil && err != syscall.ESRCH && first == nil {
			first = err
		}
	}
	return first
}

func (Procgroup) Stats(pids []int) (Stats, error) {
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
