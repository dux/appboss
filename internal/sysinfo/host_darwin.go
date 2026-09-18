//go:build darwin

package sysinfo

import (
	"strconv"
	"strings"
	"time"
)

func collectHost() Host {
	host := hostBasics()
	host.UptimeSec = darwinUptime()
	host.Load1, host.Load5, host.Load15 = darwinLoad()
	host.MemTotal, host.MemFree = darwinMem()
	host.MemUsed = host.MemTotal - host.MemFree
	return host
}

func osName() string {
	version := strings.TrimSpace(runOutput("sw_vers", "-productVersion"))
	if version == "" {
		return ""
	}
	return "macOS " + version
}

func darwinUptime() int64 {
	// sysctl prints { sec = 1700000000, usec = 0 } ...; the boot time is seconds since the epoch.
	fields := strings.Fields(runOutput("sysctl", "-n", "kern.boottime"))
	for i, field := range fields {
		if field != "sec" || i+2 >= len(fields) {
			continue
		}
		boot, err := strconv.ParseInt(strings.Trim(fields[i+2], ","), 10, 64)
		if err != nil {
			return 0
		}
		if uptime := nowUnix() - boot; uptime > 0 {
			return uptime
		}
	}
	return 0
}

func darwinLoad() (float64, float64, float64) {
	var values []float64
	for _, field := range strings.Fields(runOutput("sysctl", "-n", "vm.loadavg")) {
		if value, err := strconv.ParseFloat(strings.Trim(field, "{},"), 64); err == nil {
			values = append(values, value)
		}
	}
	if len(values) < 3 {
		return 0, 0, 0
	}
	return values[0], values[1], values[2]
}

// darwinMem reports total physical memory and an "available" figure of free, inactive and
// speculative pages, which is what macOS counts as reclaimable.
func darwinMem() (total, free int64) {
	total = parseInt(runOutput("sysctl", "-n", "hw.memsize"))
	pageSize := parseInt(runOutput("sysctl", "-n", "hw.pagesize"))
	if pageSize <= 0 {
		pageSize = 4096
	}
	var pages int64
	for _, line := range strings.Split(runOutput("vm_stat"), "\n") {
		switch {
		case strings.HasPrefix(line, "Pages free:"),
			strings.HasPrefix(line, "Pages inactive:"),
			strings.HasPrefix(line, "Pages speculative:"):
			_, rest, _ := strings.Cut(line, ":")
			pages += parseInt(strings.TrimSuffix(strings.TrimSpace(rest), "."))
		}
	}
	return total, pages * pageSize
}

func parseInt(value string) int64 {
	parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return parsed
}

func nowUnix() int64 { return time.Now().Unix() }
