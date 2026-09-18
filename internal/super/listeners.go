package super

import (
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ListenersInRange lists PIDs listening in the reserved app range without touching them, for
// `appboss doctor`.
func ListenersInRange(portRange [2]int) ([]int, error) {
	return listenerPIDs(portRange)
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

// clearPort frees a single fixed port before a process is spawned on it.
func clearPort(port int, timeout time.Duration) ([]int, error) {
	return ClearPortRange([2]int{port, port}, timeout)
}

func listenerPIDs(portRange [2]int) ([]int, error) {
	ports := strconv.Itoa(portRange[0])
	if portRange[0] != portRange[1] {
		ports += "-" + strconv.Itoa(portRange[1])
	}
	output, err := exec.Command("lsof", "-nP", "-t", "-a", "-iTCP:"+ports, "-sTCP:LISTEN").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("find listeners in port range %d-%d: %w", portRange[0], portRange[1], err)
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
