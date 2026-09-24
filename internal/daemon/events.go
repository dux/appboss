package daemon

import (
	"maps"
	"slices"

	"dboss/internal/config"
	"dboss/internal/events"
	"dboss/internal/supervisor"
)

// eventApps adapts the supervisor snapshots to the events side, which does not import the
// supervisor (the supervisor imports it for the events directory).
type eventApps struct {
	snapshots interface{ Snapshots() []supervisor.Snapshot }
}

func (e eventApps) EventApps() []events.AppConfig {
	snapshots := e.snapshots.Snapshots()
	out := make([]events.AppConfig, 0, len(snapshots))
	for _, snapshot := range snapshots {
		settings := snapshot.Web.Events
		app := events.AppConfig{Name: snapshot.Name, Retention: settings.Retention.Value()}
		for _, name := range slices.Sorted(maps.Keys(settings.Views)) {
			app.Views = append(app.Views, events.View{Name: name, Filter: settings.Views[name], Source: events.SourceYAML})
		}
		for _, name := range slices.Sorted(maps.Keys(settings.Funnels)) {
			app.Funnels = append(app.Funnels, config.EventFunnelOf(name, settings.Funnels[name]))
		}
		out = append(out, app)
	}
	return out
}
