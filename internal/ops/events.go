package ops

import (
	"context"
	"encoding/json"
	"errors"

	"dboss/internal/events"
)

var errNoEvents = errors.New("events are not available")

// SetEvents attaches the events service; without it every events action fails.
func (s *Service) SetEvents(service *events.Service) { s.events = service }

// EventSummary counts an app's events per day and event over a filter.
func (s *Service) EventSummary(app, filter string) (events.Summary, error) {
	if s.events == nil {
		return events.Summary{}, errNoEvents
	}
	return s.events.Summary(app, filter)
}

// LatestEvents lists the newest events matching a filter.
func (s *Service) LatestEvents(app, filter string, limit int) ([]events.Event, error) {
	if s.events == nil {
		return nil, errNoEvents
	}
	if limit <= 0 {
		limit = 50
	}
	return s.events.Latest(app, filter, limit)
}

// EventFacets lists the values of a tag key, the labels (#), the tag keys ("") or the data keys
// (data.) among the matching events.
func (s *Service) EventFacets(app, filter, key string) ([]events.Facet, error) {
	if s.events == nil {
		return nil, errNoEvents
	}
	return s.events.Facets(app, filter, key)
}

// EventViewsResult is an app's saved views and funnels with where its files are and whether
// DuckDB is there for SQL and funnels.
type EventViewsResult struct {
	events.Catalog
	Status events.Status `json:"status"`
}

func (s *Service) EventViews(app string) (EventViewsResult, error) {
	if s.events == nil {
		return EventViewsResult{}, errNoEvents
	}
	catalog, err := s.events.Catalog(app)
	if err != nil {
		return EventViewsResult{}, err
	}
	status, err := s.events.Status(context.Background(), app)
	return EventViewsResult{Catalog: catalog, Status: status}, err
}

// RunFunnel runs a saved funnel by name, or the funnel definition in data when it is given (the
// console's unsaved builder). filter narrows step 1.
func (s *Service) RunFunnel(app, name string, data json.RawMessage, filter string) (events.QueryResult, error) {
	if s.events == nil {
		return events.QueryResult{}, errNoEvents
	}
	var funnel *events.Funnel
	if len(data) > 0 && string(data) != "null" {
		funnel = &events.Funnel{}
		if err := json.Unmarshal(data, funnel); err != nil {
			return events.QueryResult{}, err
		}
		if funnel.Name == "" {
			funnel.Name = "draft"
		}
	}
	return s.events.RunFunnel(context.Background(), app, name, funnel, filter)
}

func (s *Service) eventsQuery(app, sql string) (events.QueryResult, error) {
	if s.events == nil {
		return events.QueryResult{}, errNoEvents
	}
	return s.events.Query(context.Background(), app, sql)
}

// eventsSave stores a console view or funnel; data is its JSON.
func (s *Service) eventsSave(app, kind string, data json.RawMessage) error {
	if s.events == nil {
		return errNoEvents
	}
	switch kind {
	case "view":
		var view events.View
		if err := json.Unmarshal(data, &view); err != nil {
			return err
		}
		return s.events.SaveView(app, view)
	case "funnel":
		var funnel events.Funnel
		if err := json.Unmarshal(data, &funnel); err != nil {
			return err
		}
		return s.events.SaveFunnel(app, funnel)
	}
	return errors.New("kind is view or funnel")
}

func (s *Service) eventsDelete(app, kind, name string) error {
	if s.events == nil {
		return errNoEvents
	}
	return s.events.Delete(app, kind, name)
}

// savedName is the name inside a saved view or funnel, for the audit row.
func savedName(data json.RawMessage) string {
	var named struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(data, &named)
	return named.Name
}
