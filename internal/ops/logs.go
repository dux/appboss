package ops

import (
	"errors"
	"slices"
	"time"

	"dboss/internal/logstore"
)

// errNoLogStore answers the log viewer while the store is off.
var errNoLogStore = errors.New("log store is not enabled")

// SearchLogs and SearchRequests are the read side of the log store, shared by the console and
// any future `dboss logs --search`.
func (s *Service) SearchLogs(app string, filter logstore.LogFilter) ([]logstore.LogEntry, error) {
	if s.store == nil {
		return nil, errNoLogStore
	}
	return s.store.SearchLogs(app, filter)
}

func (s *Service) SearchRequests(app string, filter logstore.RequestFilter) ([]logstore.RequestEntry, error) {
	if s.store == nil {
		return nil, errNoLogStore
	}
	return s.store.SearchRequests(app, filter)
}

// Channels lists the log types an app has for the console viewer.
func (s *Service) Channels(app string) ([]logstore.Channel, error) {
	if s.store == nil {
		return nil, errNoLogStore
	}
	return s.store.Channels(app)
}

// LogTree is the log viewer's left nav: every app, sqlite size on disk, and the channels inside.
func (s *Service) LogTree() ([]logstore.AppTree, error) {
	if s.store == nil {
		return nil, errNoLogStore
	}
	names := make([]string, 0, len(s.runtime.Snapshots())+1)
	for _, snapshot := range s.runtime.Snapshots() {
		names = append(names, snapshot.Name)
	}
	slices.Sort(names)
	names = append(names, logstore.HostApp)
	return s.store.Tree(names)
}

// Blocked lists the deny counters from the reserved host database. A store without the capability
// (a test double) answers empty.
func (s *Service) Blocked() ([]logstore.BlockedStat, error) {
	if s.store == nil {
		return nil, errNoLogStore
	}
	reader, ok := s.store.(interface {
		Blocked() ([]logstore.BlockedStat, error)
	})
	if !ok {
		return nil, nil
	}
	return reader.Blocked()
}

// Traffic returns the aggregated request log of one app for requests newer than since.
func (s *Service) Traffic(name string, since time.Time) (logstore.Traffic, error) {
	if s.store == nil {
		return logstore.Traffic{}, errors.New("traffic is not available")
	}
	return s.store.Traffic(name, since)
}

// FleetSeries is the request count per bucket over every app, for the overview chart.
func (s *Service) FleetSeries(since time.Time) ([]logstore.TrafficBucket, error) {
	if s.store == nil {
		return nil, errors.New("traffic is not available")
	}
	snapshots := s.runtime.Snapshots()
	names := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		names = append(names, snapshot.Name)
	}
	return s.store.Series(names, since)
}
