package ports

import (
	"errors"
	"sync"
)

// Allocator hands out one fixed port per (app, process) for the lifetime of the daemon.
// Entries are never reassigned. A host counts up from the start of its range, which is cleared
// with lsof before apps start; a dev session takes each port from claim instead.
type Allocator struct {
	mu      sync.Mutex
	first   int
	last    int
	claim   func(key string) (int, error)
	entries map[string]int
}

func New(portRange [2]int) *Allocator {
	return &Allocator{first: portRange[0], last: portRange[1], entries: map[string]int{}}
}

// NewClaiming is the dev session allocator: claim (Registry.Claim) picks every port, so
// sessions in other app folders never get the same one.
func NewClaiming(claim func(key string) (int, error)) *Allocator {
	return &Allocator{claim: claim, entries: map[string]int{}}
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
	if a.claim != nil {
		port, err := a.claim(key)
		if err != nil {
			return 0, err
		}
		a.entries[key] = port
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
