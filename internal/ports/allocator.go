package ports

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type Allocator struct {
	mu         sync.Mutex
	path       string
	first      int
	last       int
	checkBound bool
	entries    map[string]int
}

func Open(stateDir string, portRange [2]int, checkBound bool) (*Allocator, error) {
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return nil, err
	}
	a := &Allocator{path: filepath.Join(stateDir, "ports.json"), first: portRange[0], last: portRange[1], checkBound: checkBound, entries: map[string]int{}}
	data, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &a.entries); err != nil {
		return nil, fmt.Errorf("decode ports.json: %w", err)
	}
	seen := map[int]string{}
	for key, port := range a.entries {
		if port < a.first || port > a.last {
			return nil, fmt.Errorf("ports.json: %s uses %d outside configured range", key, port)
		}
		if owner := seen[port]; owner != "" {
			return nil, fmt.Errorf("ports.json: %s and %s both use %d", owner, key, port)
		}
		seen[port] = key
	}
	return a, nil
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
		if used[port] || (a.checkBound && isBound(port)) {
			continue
		}
		a.entries[key] = port
		if err := a.save(); err != nil {
			delete(a.entries, key)
			return 0, err
		}
		return port, nil
	}
	return 0, errors.New("port range exhausted")
}

func (a *Allocator) Release(app string, stopped bool) error {
	if !stopped {
		return errors.New("app must be stopped before releasing ports")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	prefix := app + "/"
	changed := false
	for key := range a.entries {
		if strings.HasPrefix(key, prefix) {
			delete(a.entries, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return a.save()
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

func (a *Allocator) save() error {
	keys := make([]string, 0, len(a.entries))
	for key := range a.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered := make(map[string]int, len(keys))
	for _, key := range keys {
		ordered[key] = a.entries[key]
	}
	data, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	newPath := a.path + ".new"
	if err := os.WriteFile(newPath, data, 0o640); err != nil {
		return err
	}
	return os.Rename(newPath, a.path)
}

func isBound(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	_ = listener.Close()
	return false
}
