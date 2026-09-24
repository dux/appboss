package events

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"dboss/internal/fsutil"
)

// AppConfig is what the events side needs of one app: its retention and the views and funnels
// its dboss.yaml declares. The daemon builds it from the supervisor snapshots.
type AppConfig struct {
	Name      string
	Retention time.Duration // 0 means events are off
	Views     []View
	Funnels   []Funnel
}

// Apps lists the apps the events module and readers serve.
type Apps interface {
	EventApps() []AppConfig
}

// Saved is what the console saved for one app, next to what dboss.yaml declares.
type Saved struct {
	Views   []View   `json:"views"`
	Funnels []Funnel `json:"funnels"`
}

// SavedStore keeps the console-saved views and funnels at dir/state/<app>/events-views.json.
type SavedStore struct {
	stateDir string
	mu       sync.Mutex
}

func NewSavedStore(stateDir string) *SavedStore { return &SavedStore{stateDir: stateDir} }

func (s *SavedStore) path(app string) string {
	return filepath.Join(s.stateDir, app, "events-views.json")
}

// Load reads an app's saved views; a missing file is empty.
func (s *SavedStore) Load(app string) (Saved, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var saved Saved
	err := fsutil.ReadJSON(s.path(app), &saved)
	return saved, err
}

// update loads, changes and writes an app's saved views under one lock.
func (s *SavedStore) update(app string, change func(*Saved) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var saved Saved
	if err := fsutil.ReadJSON(s.path(app), &saved); err != nil {
		return err
	}
	if err := change(&saved); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path(app)), 0o755); err != nil {
		return err
	}
	return fsutil.WriteJSON(s.path(app), saved, 0o644)
}

// SaveView adds or replaces a console view.
func (s *SavedStore) SaveView(app string, view View) error {
	view.Source = SourceConsole
	view.Filter = strings.TrimSpace(view.Filter)
	if err := ValidateView(view); err != nil {
		return err
	}
	return s.update(app, func(saved *Saved) error {
		saved.Views = slices.DeleteFunc(saved.Views, func(v View) bool { return v.Name == view.Name })
		saved.Views = append(saved.Views, view)
		slices.SortFunc(saved.Views, func(a, b View) int { return strings.Compare(a.Name, b.Name) })
		return nil
	})
}

// SaveFunnel adds or replaces a console funnel.
func (s *SavedStore) SaveFunnel(app string, funnel Funnel) error {
	funnel.Source = SourceConsole
	if err := ValidateFunnel(&funnel); err != nil {
		return err
	}
	return s.update(app, func(saved *Saved) error {
		saved.Funnels = slices.DeleteFunc(saved.Funnels, func(f Funnel) bool { return f.Name == funnel.Name })
		saved.Funnels = append(saved.Funnels, funnel)
		slices.SortFunc(saved.Funnels, func(a, b Funnel) int { return strings.Compare(a.Name, b.Name) })
		return nil
	})
}

// Delete removes a console view or funnel by name. A name only dboss.yaml declares cannot be
// deleted here.
func (s *SavedStore) Delete(app, kind, name string) error {
	return s.update(app, func(saved *Saved) error {
		before := len(saved.Views) + len(saved.Funnels)
		switch kind {
		case "view":
			saved.Views = slices.DeleteFunc(saved.Views, func(v View) bool { return v.Name == name })
		case "funnel":
			saved.Funnels = slices.DeleteFunc(saved.Funnels, func(f Funnel) bool { return f.Name == name })
		default:
			return fmt.Errorf("unknown kind %q: view or funnel", kind)
		}
		if len(saved.Views)+len(saved.Funnels) == before {
			return fmt.Errorf("no console %s named %s", kind, name)
		}
		return nil
	})
}

// Catalog is the merged list an app offers: dboss.yaml entries first-class, console entries next
// to them, and the console names a yaml entry hides.
type Catalog struct {
	Views    []View   `json:"views"`
	Funnels  []Funnel `json:"funnels"`
	Shadowed []string `json:"shadowed,omitempty"`
}

// Merge combines an app's yaml and console entries. A yaml entry wins a name clash; the console
// entry it hides is reported so the console can say so.
func Merge(app AppConfig, saved Saved) Catalog {
	var catalog Catalog
	names := map[string]bool{}
	for _, view := range app.Views {
		view.Source = SourceYAML
		catalog.Views = append(catalog.Views, view)
		names[view.Name] = true
	}
	for _, funnel := range app.Funnels {
		funnel.Source = SourceYAML
		catalog.Funnels = append(catalog.Funnels, funnel)
		names[funnel.Name] = true
	}
	for _, view := range saved.Views {
		if names[view.Name] {
			catalog.Shadowed = append(catalog.Shadowed, view.Name)
			continue
		}
		catalog.Views = append(catalog.Views, view)
	}
	for _, funnel := range saved.Funnels {
		if names[funnel.Name] {
			catalog.Shadowed = append(catalog.Shadowed, funnel.Name)
			continue
		}
		catalog.Funnels = append(catalog.Funnels, funnel)
	}
	slices.SortFunc(catalog.Views, func(a, b View) int {
		return cmp.Or(strings.Compare(a.Source, b.Source)*-1, strings.Compare(a.Name, b.Name))
	})
	slices.SortFunc(catalog.Funnels, func(a, b Funnel) int {
		return cmp.Or(strings.Compare(a.Source, b.Source)*-1, strings.Compare(a.Name, b.Name))
	})
	return catalog
}

// ViewsFile is where an app's views.sql lives.
func (s *Store) ViewsFile(app string) string { return filepath.Join(s.Dir(app), "views.sql") }

// WriteViews writes views.sql for readers outside dboss (the app, a duckdb shell): the base views
// plus every saved view and funnel, with absolute paths. It rewrites only on a change and does
// nothing for an app without an events directory.
func (s *Store) WriteViews(app string, catalog Catalog) error {
	if _, err := os.Stat(s.Dir(app)); err != nil {
		return nil
	}
	base, err := s.BaseViews(app)
	if err != nil {
		return err
	}
	content := "-- Written by dboss; regenerated when events, views or funnels change. Load it with\n" +
		"-- .read views.sql in the duckdb shell, or run it on a DuckDB connection.\n" +
		base + SavedViews(catalog.Views, catalog.Funnels)
	path := s.ViewsFile(app)
	if current, err := os.ReadFile(path); err == nil && string(current) == content {
		return nil
	}
	return fsutil.WriteFile(path, []byte(content), 0o644)
}
