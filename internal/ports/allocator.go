package ports

import (
	"errors"
	"sync"
)

// Allocator hands out one fixed port per (app, process) for the lifetime of the daemon.
// Entries are never reassigned; the whole range is cleared with lsof before apps start.
type Allocator struct {
	mu      sync.Mutex
	first   int
	last    int
	entries map[string]int
}

func New(portRange [2]int) *Allocator {
	return &Allocator{first: portRange[0], last: portRange[1], entries: map[string]int{}}
}

func (a *Allocator) Lookup(app, process string) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	port, ok := a.entries[app+"/"+process]
	return port, ok
}

func (a *Allocator) Allocate(app, process string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := app + "/" + process
	if port, ok := a.entries[key]; ok {
		return port, nil
	}
	used := map[int]bool{}
	for _, port := range a.entries {
		used[port] = true
	}
	for port := a.first; port <= a.last; port++ {
		if used[port] {
			continue
		}
		a.entries[key] = port
		return port, nil
	}
	return 0, errors.New("port range exhausted")
}

func (a *Allocator) Entries() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make(map[string]int, len(a.entries))
	for key, value := range a.entries {
		result[key] = value
	}
	return result
}
