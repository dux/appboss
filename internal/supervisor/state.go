package supervisor

import (
	"maps"
	"path/filepath"
	"slices"
	"time"

	"dboss/internal/fsutil"
	"dboss/internal/logx"
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
	delete(m.created, name)
	if err := saveNames(filepath.Join(m.cfg.StateDir, "created.json"), m.created); err != nil {
		return err
	}
	delete(m.maintenance, name)
	return saveNames(filepath.Join(m.cfg.StateDir, "maintenance.json"), m.maintenance)
}

// markCreated records that name's create step succeeded, so it never runs again for that app.
func (m *Manager) markCreated(name string) {
	m.desiredMu.Lock()
	defer m.desiredMu.Unlock()
	m.created[name] = true
	if err := saveNames(filepath.Join(m.cfg.StateDir, "created.json"), m.created); err != nil {
		logx.Warnf("%s: record lifecycle create: %v", name, err)
	}
}

// saveNames writes the sorted keys of set to path as a JSON list.
func saveNames(path string, set map[string]bool) error {
	return fsutil.WriteJSON(path, slices.Sorted(maps.Keys(set)), 0o640)
}

func loadNames(path string) (map[string]bool, error) {
	result := map[string]bool{}
	var names []string
	if err := fsutil.ReadJSON(path, &names); err != nil {
		return nil, err
	}
	for _, name := range names {
		result[name] = true
	}
	return result, nil
}

func loadActivities(stateDir string) (map[string]time.Time, error) {
	result := map[string]time.Time{}
	if err := fsutil.ReadJSON(filepath.Join(stateDir, "last_activity.json"), &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (m *Manager) saveActivities() error {
	runtimes := m.runtimes()
	activities := map[string]time.Time{}
	for _, runtime := range runtimes {
		snapshot := runtime.query(request{kind: requestSnapshot}).snapshot
		if !snapshot.LastActivity.IsZero() {
			activities[snapshot.Name] = snapshot.LastActivity
		}
	}
	return fsutil.WriteJSON(filepath.Join(m.cfg.StateDir, "last_activity.json"), activities, 0o640)
}
