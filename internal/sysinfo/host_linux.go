//go:build linux

package sysinfo

import (
	"os"
	"strconv"
	"strings"
)

func collectHost() Host {
	host := hostBasics()
	host.UptimeSec = linuxUptime()
	host.Load1, host.Load5, host.Load15 = linuxLoad()
	host.MemTotal, host.MemFree, host.SwapTotal, host.SwapFree = linuxMem()
	host.MemUsed = host.MemTotal - host.MemFree
	return host
}

func osName() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ""
}

func linuxUptime() int64 {
	fields := strings.Fields(readProc("/proc/uptime"))
	if len(fields) == 0 {
		return 0
	}
	seconds, _ := strconv.ParseFloat(fields[0], 64)
	return int64(seconds)
}

func linuxLoad() (float64, float64, float64) {
	fields := strings.Fields(readProc("/proc/loadavg"))
	if len(fields) < 3 {
		return 0, 0, 0
	}
	first, _ := strconv.ParseFloat(fields[0], 64)
	five, _ := strconv.ParseFloat(fields[1], 64)
	fifteen, _ := strconv.ParseFloat(fields[2], 64)
	return first, five, fifteen
}

// linuxMem reads /proc/meminfo and returns bytes. MemAvailable is the free figure because it is
// the amount a new process can actually claim, unlike MemFree.
func linuxMem() (total, free, swapTotal, swapFree int64) {
	values := map[string]int64{}
	for _, line := range strings.Split(readProc("/proc/meminfo"), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		amount, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		values[key] = amount * 1024
	}
	return values["MemTotal"], values["MemAvailable"], values["SwapTotal"], values["SwapFree"]
}

func readProc(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
