package ports

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Listener is one process holding a port in the reserved app range, with the address it bound.
// The address is the app's own choice: dboss injects PORT and never an interface.
type Listener struct {
	PID     int    `json:"pid"`
	Command string `json:"command"`
	Address string `json:"address"`
}

// Loopback is true when only the box itself can reach the listener. lsof prints a socket bound
// to every interface as "*:3101" and the v6 wildcard as "[::]:3101", neither of which parses as
// an address, so anything but a real loopback IP counts as reachable from outside.
func (l Listener) Loopback() bool {
	host := l.Address
	if index := strings.LastIndex(host, ":"); index > 0 {
		host = host[:index]
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// ListenersInRange lists what is listening in the reserved app range without touching it, for
// `dboss doctor`. One entry per socket, so a process on two addresses shows up twice.
func ListenersInRange(portRange [2]int) ([]Listener, error) {
	output, err := lsofRange(portRange, "-Fpcn")
	if err != nil || len(output) == 0 {
		return nil, err
	}
	var listeners []Listener
	var current Listener
	for _, line := range strings.Split(string(output), "\n") {
		if len(line) < 2 {
			continue
		}
		field, value := line[0], line[1:]
		switch field {
		case 'p':
			pid, parseErr := strconv.Atoi(value)
			if parseErr != nil || pid <= 1 {
				return nil, fmt.Errorf("invalid listener pid %q from lsof", value)
			}
			current = Listener{PID: pid}
		case 'c':
			current.Command = value
		case 'n':
			entry := current
			entry.Address = value
			listeners = append(listeners, entry)
		}
	}
	sort.Slice(listeners, func(a, b int) bool {
		if listeners[a].PID != listeners[b].PID {
			return listeners[a].PID < listeners[b].PID
		}
		return listeners[a].Address < listeners[b].Address
	})
	return listeners, nil
}

// ClearPortRange terminates every process group listening in the reserved app range.
func ClearPortRange(portRange [2]int, timeout time.Duration) ([]int, error) {
	pids, err := listenerPIDs(portRange)
	if err != nil || len(pids) == 0 {
		return pids, err
	}
	if err := signalListenerGroups(pids, syscall.SIGTERM); err != nil {
		return pids, err
	}
	if waitForClearPortRange(portRange, timeout) {
		return pids, nil
	}
	remaining, err := listenerPIDs(portRange)
	if err != nil {
		return pids, err
	}
	if err := signalListenerGroups(remaining, syscall.SIGKILL); err != nil {
		return pids, err
	}
	if !waitForClearPortRange(portRange, 2*time.Second) {
		return pids, fmt.Errorf("listeners remain in port range %d-%d", portRange[0], portRange[1])
	}
	return pids, nil
}

// ClearPort frees a single fixed port before a process is spawned on it.
func ClearPort(port int, timeout time.Duration) ([]int, error) {
	return ClearPortRange([2]int{port, port}, timeout)
}

// lsofRange runs one listener query over the range; lsof exits 1 when nothing matches, which is
// an empty answer rather than a failure.
func lsofRange(portRange [2]int, args ...string) ([]byte, error) {
	ports := strconv.Itoa(portRange[0])
	if portRange[0] != portRange[1] {
		ports += "-" + strconv.Itoa(portRange[1])
	}
	full := append([]string{"-nP"}, args...)
	full = append(full, "-a", "-iTCP:"+ports, "-sTCP:LISTEN")
	output, err := exec.Command("lsof", full...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("find listeners in port range %d-%d: %w", portRange[0], portRange[1], err)
	}
	return output, nil
}

func listenerPIDs(portRange [2]int) ([]int, error) {
	output, err := lsofRange(portRange, "-t")
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	for _, value := range strings.Fields(string(output)) {
		pid, parseErr := strconv.Atoi(value)
		if parseErr != nil || pid <= 1 {
			return nil, fmt.Errorf("invalid listener pid %q from lsof", value)
		}
		seen[pid] = true
	}
	pids := make([]int, 0, len(seen))
	for pid := range seen {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}

func signalListenerGroups(pids []int, signal syscall.Signal) error {
	targets := map[int]bool{}
	currentGroup := syscall.Getpgrp()
	for _, pid := range pids {
		target := pid
		if group, err := syscall.Getpgid(pid); err == nil && group > 1 && group != currentGroup {
			target = -group
		}
		targets[target] = true
	}
	for target := range targets {
		if err := syscall.Kill(target, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signal listener target %d: %w", target, err)
		}
	}
	return nil
}

func waitForClearPortRange(portRange [2]int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		pids, err := listenerPIDs(portRange)
		if err == nil && len(pids) == 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}
