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

// Exceptions lists one app's aggregated exception groups. A store without the capability (a test
// double) answers empty.
func (s *Service) Exceptions(app string, filter logstore.ExceptionFilter) ([]logstore.ExceptionSummary, error) {
	if s.store == nil {
		return nil, errNoLogStore
	}
	reader, ok := s.store.(interface {
		Exceptions(string, logstore.ExceptionFilter) ([]logstore.ExceptionSummary, error)
	})
	if !ok {
		return nil, nil
	}
	return reader.Exceptions(app, filter)
}

// UnresolvedExceptions counts one app's unresolved exception groups for the app card badge. A
// store without the capability (a test double) answers zero.
func (s *Service) UnresolvedExceptions(app string) (int, error) {
	if s.store == nil {
		return 0, nil
	}
	reader, ok := s.store.(interface {
		UnresolvedExceptionCount(string) (int, error)
	})
	if !ok {
		return 0, nil
	}
	return reader.UnresolvedExceptionCount(app)
}

// resolveException flips the resolved flag on one exception group. A store without the capability
// (a test double) answers with an error, so a click never silently no-ops.
func (s *Service) resolveException(app, expUID string, resolved bool) error {
	if s.store == nil {
		return errNoLogStore
	}
	writer, ok := s.store.(interface {
		SetExceptionResolved(string, string, bool) error
	})
	if !ok {
		return errors.New("exceptions are not available")
	}
	return writer.SetExceptionResolved(app, expUID, resolved)
}

// ignoreException marks one exception group ignored (and therefore resolved) or clears only the
// ignore flag. A store without the capability answers with an error.
func (s *Service) ignoreException(app, expUID string, ignored bool) error {
	if s.store == nil {
		return errNoLogStore
	}
	writer, ok := s.store.(interface {
		SetExceptionIgnored(string, string, bool) error
	})
	if !ok {
		return errors.New("exceptions are not available")
	}
	return writer.SetExceptionIgnored(app, expUID, ignored)
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
