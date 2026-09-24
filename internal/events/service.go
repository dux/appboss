package events

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Service is the events surface the ops layer serves to the CLI and the console: reads from the
// files, DuckDB runs for SQL and funnels, and the console-saved views.
type Service struct {
	Store  *Store
	Reader *Reader
	Saved  *SavedStore
	Apps   Apps

	duckMu      sync.Mutex
	duck        DuckDB
	duckErr     error
	duckChecked time.Time
}

// duckCheck is how long a duckdb lookup is trusted, so the console does not start
// `duckdb --version` on every tab switch while an install still shows up within a minute.
const duckCheck = time.Minute

// DuckDB finds the duckdb CLI, remembering the answer for duckCheck.
func (s *Service) DuckDB(ctx context.Context) (DuckDB, error) {
	s.duckMu.Lock()
	defer s.duckMu.Unlock()
	if time.Since(s.duckChecked) > duckCheck {
		s.duck, s.duckErr = FindDuckDB(ctx)
		s.duckChecked = time.Now()
	}
	return s.duck, s.duckErr
}

func NewService(store *Store, saved *SavedStore, apps Apps) *Service {
	return &Service{Store: store, Reader: NewReader(store), Saved: saved, Apps: apps}
}

// App returns one configured app's events config.
func (s *Service) App(name string) (AppConfig, error) {
	for _, app := range s.Apps.EventApps() {
		if app.Name == name {
			return app, nil
		}
	}
	return AppConfig{}, fmt.Errorf("unknown app %q", name)
}

// Catalog is an app's saved views and funnels, dboss.yaml and console merged.
func (s *Service) Catalog(name string) (Catalog, error) {
	app, err := s.App(name)
	if err != nil {
		return Catalog{}, err
	}
	saved, err := s.Saved.Load(name)
	if err != nil {
		return Catalog{}, err
	}
	return Merge(app, saved), nil
}

// views is the SQL a run starts from: the base views and the saved ones.
func (s *Service) views(name string) (string, Catalog, error) {
	catalog, err := s.Catalog(name)
	if err != nil {
		return "", Catalog{}, err
	}
	base, err := s.Store.BaseViews(name)
	if err != nil {
		return "", Catalog{}, err
	}
	return base + SavedViews(catalog.Views, catalog.Funnels), catalog, nil
}

// Query runs SQL against an app's views in the sandbox.
func (s *Service) Query(ctx context.Context, name, sql string) (QueryResult, error) {
	duck, err := s.DuckDB(ctx)
	if err != nil {
		return QueryResult{}, err
	}
	views, _, err := s.views(name)
	if err != nil {
		return QueryResult{}, err
	}
	return duck.Query(ctx, s.Store.Dir(name), views, sql)
}

// RunFunnel runs a saved funnel by name, or funnel when it is given (the console's unsaved
// builder). extra narrows step 1, typically a time range.
func (s *Service) RunFunnel(ctx context.Context, name, funnelName string, funnel *Funnel, extra string) (QueryResult, error) {
	filter, err := ParseFilter(extra)
	if err != nil {
		return QueryResult{}, err
	}
	duck, err := s.DuckDB(ctx)
	if err != nil {
		return QueryResult{}, err
	}
	views, catalog, err := s.views(name)
	if err != nil {
		return QueryResult{}, err
	}
	if funnel == nil {
		index := slices.IndexFunc(catalog.Funnels, func(f Funnel) bool { return f.Name == funnelName })
		if index < 0 {
			return QueryResult{}, fmt.Errorf("no funnel named %q", funnelName)
		}
		funnel = &catalog.Funnels[index]
	}
	query, err := FunnelSQL(*funnel, filter)
	if err != nil {
		return QueryResult{}, err
	}
	return duck.Query(ctx, s.Store.Dir(name), views, query)
}

// SaveView stores a console view and refreshes views.sql.
func (s *Service) SaveView(name string, view View) error {
	if _, err := s.App(name); err != nil {
		return err
	}
	if err := s.Saved.SaveView(name, view); err != nil {
		return err
	}
	return s.refresh(name)
}

// SaveFunnel stores a console funnel and refreshes views.sql.
func (s *Service) SaveFunnel(name string, funnel Funnel) error {
	if _, err := s.App(name); err != nil {
		return err
	}
	if err := s.Saved.SaveFunnel(name, funnel); err != nil {
		return err
	}
	return s.refresh(name)
}

// Delete removes a console view or funnel and refreshes views.sql.
func (s *Service) Delete(name, kind, entry string) error {
	if _, err := s.App(name); err != nil {
		return err
	}
	if err := s.Saved.Delete(name, kind, entry); err != nil {
		return err
	}
	return s.refresh(name)
}

func (s *Service) refresh(name string) error {
	catalog, err := s.Catalog(name)
	if err != nil {
		return err
	}
	return s.Store.WriteViews(name, catalog)
}

// Summary, Latest and Facets parse the filter and read the files.
func (s *Service) Summary(name, filter string) (Summary, error) {
	parsed, err := s.filter(name, filter)
	if err != nil {
		return Summary{}, err
	}
	return s.Reader.Summary(name, parsed, time.Now())
}

func (s *Service) Latest(name, filter string, limit int) ([]Event, error) {
	parsed, err := s.filter(name, filter)
	if err != nil {
		return nil, err
	}
	return s.Reader.Latest(name, parsed, limit, time.Now())
}

func (s *Service) Facets(name, filter, key string) ([]Facet, error) {
	parsed, err := s.filter(name, filter)
	if err != nil {
		return nil, err
	}
	return s.Reader.Facets(name, parsed, strings.TrimSpace(key), time.Now())
}

func (s *Service) filter(name, text string) (Filter, error) {
	if _, err := s.App(name); err != nil {
		return Filter{}, err
	}
	return ParseFilter(text)
}

// Status says whether SQL and funnels are available and where an app's files are.
type Status struct {
	Dir      string `json:"dir"`
	DuckDB   string `json:"duckdb,omitempty"`
	Missing  string `json:"missing,omitempty"`
	Enabled  bool   `json:"enabled"`
	ViewsSQL string `json:"views_sql"`
}

func (s *Service) Status(ctx context.Context, name string) (Status, error) {
	app, err := s.App(name)
	if err != nil {
		return Status{}, err
	}
	status := Status{Dir: s.Store.Dir(name), Enabled: app.Retention > 0, ViewsSQL: s.Store.ViewsFile(name)}
	if duck, err := s.DuckDB(ctx); err == nil {
		status.DuckDB = duck.Version
	} else {
		status.Missing = err.Error()
	}
	return status, nil
}
