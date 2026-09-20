package super

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"
)

func (m *Manager) setDesired(name string, running bool) error {
	m.desiredMu.Lock()
	defer m.desiredMu.Unlock()
	if running {
		m.desired[name] = true
	} else {
		delete(m.desired, name)
	}
	return saveNames(filepath.Join(m.cfg.StateDir, "running.json"), m.desired)
}

func (m *Manager) clearAppState(name string) error {
	m.desiredMu.Lock()
	defer m.desiredMu.Unlock()
	delete(m.desired, name)
	if err := saveNames(filepath.Join(m.cfg.StateDir, "running.json"), m.desired); err != nil {
		return err
	}
	delete(m.maintenance, name)
	return saveNames(filepath.Join(m.cfg.StateDir, "maintenance.json"), m.maintenance)
}

// saveNames writes the sorted keys of set to path as a JSON list.
func saveNames(path string, set map[string]bool) error {
	return writeStateFile(path, slices.Sorted(maps.Keys(set)))
}

// writeStateFile marshals value as indented JSON and replaces path atomically, so a crash never
// leaves a half-written state file behind.
func writeStateFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	newPath := path + ".new"
	if err := os.WriteFile(newPath, data, 0o640); err != nil {
		return err
	}
	return os.Rename(newPath, path)
}

// cronLoop asks every app for due jobs on a short tick; jobs run whatever the app's own state is.

func loadNames(path string) (map[string]bool, error) {
	result := map[string]bool{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, err
	}
	for _, name := range names {
		result[name] = true
	}
	return result, nil
}

func loadActivities(stateDir string) (map[string]time.Time, error) {
	result := map[string]time.Time{}
	data, err := os.ReadFile(filepath.Join(stateDir, "last_activity.json"))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (m *Manager) saveActivities() error {
	m.mu.RLock()
	runtimes := make([]*appRuntime, 0, len(m.apps))
	for _, runtime := range m.apps {
		runtimes = append(runtimes, runtime)
	}
	m.mu.RUnlock()
	activities := map[string]time.Time{}
	for _, runtime := range runtimes {
		snapshot := runtime.query(request{kind: requestSnapshot}).snapshot
		if !snapshot.LastActivity.IsZero() {
			activities[snapshot.Name] = snapshot.LastActivity
		}
	}
	return writeStateFile(filepath.Join(m.cfg.StateDir, "last_activity.json"), activities)
}
