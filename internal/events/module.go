package events

import (
	"context"
	"sync"
	"time"

	"dboss/internal/logx"
	"dboss/internal/schedule"
)

// viewsInterval is how often views.sql is brought up to date, so a view over a dataset that
// was empty becomes a real one soon after the first file lands.
const viewsInterval = time.Minute

// Module runs the daily maintenance of the events store (compaction, then retention) at
// maintenance_at, once shortly after start to catch up on days the daemon was down for, and keeps
// every app's views.sql current.
type Module struct {
	store            *Store
	saved            *SavedStore
	apps             Apps
	maintenanceAt    string
	defaultRetention time.Duration
	cancel           context.CancelFunc
	wg               sync.WaitGroup
}

// NewModule builds the events module. defaultRetention applies to the files of an app that is no
// longer in the config.
func NewModule(store *Store, saved *SavedStore, apps Apps, maintenanceAt string, defaultRetention time.Duration) *Module {
	return &Module{store: store, saved: saved, apps: apps, maintenanceAt: maintenanceAt, defaultRetention: defaultRetention}
}

func (m *Module) Name() string { return "events" }

func (m *Module) Start(ctx context.Context) error {
	ctx, m.cancel = context.WithCancel(ctx)
	m.wg.Add(2)
	go func() {
		defer m.wg.Done()
		m.Maintain(time.Now())
		schedule.Daily(ctx, m.maintenanceAt, func() { m.Maintain(time.Now()) })
	}()
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(viewsInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.RefreshViews()
			}
		}
	}()
	return nil
}

func (m *Module) Close() error {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	return nil
}

// Maintain compacts closed days and prunes raw days past retention for every app with events on
// disk, including apps removed from the config, then refreshes views.sql.
func (m *Module) Maintain(now time.Time) {
	configured := map[string]AppConfig{}
	for _, app := range m.apps.EventApps() {
		configured[app.Name] = app
	}
	names, err := m.store.Apps()
	if err != nil {
		logx.Warnf("events: list apps: %v", err)
		return
	}
	for _, name := range names {
		if err := m.store.CompactApp(name, now); err != nil {
			logx.Warnf("events: %v", err)
		}
		retention := m.defaultRetention
		if app, ok := configured[name]; ok {
			retention = app.Retention
		}
		if err := m.store.Prune(name, retention, now); err != nil {
			logx.Warnf("events: prune %s: %v", name, err)
		}
	}
	m.RefreshViews()
}

// RefreshViews rewrites the views.sql of every configured app whose views changed.
func (m *Module) RefreshViews() {
	for _, app := range m.apps.EventApps() {
		if err := m.RefreshApp(app); err != nil {
			logx.Warnf("events: views %s: %v", app.Name, err)
		}
	}
}

// RefreshApp rewrites one app's views.sql, after a console save or a config change.
func (m *Module) RefreshApp(app AppConfig) error {
	saved, err := m.saved.Load(app.Name)
	if err != nil {
		return err
	}
	return m.store.WriteViews(app.Name, Merge(app, saved))
}
