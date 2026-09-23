package ops

import (
	"errors"
	"time"

	"dboss/internal/diskusage"
	"dboss/internal/supervisor"
)

// Apps returns every app snapshot with its request rates and disk usage filled in.
func (s *Service) Apps() []supervisor.Snapshot {
	snapshots := s.runtime.Snapshots()
	for i := range snapshots {
		s.decorate(&snapshots[i])
	}
	return snapshots
}

func (s *Service) app(name string) (supervisor.Snapshot, error) {
	snapshot, err := s.runtime.Snapshot(name)
	if err != nil {
		return snapshot, err
	}
	s.decorate(&snapshot)
	return snapshot, nil
}

// DiskRefresh measures one app on demand. It only reads the filesystem, so it writes no audit row.
func (s *Service) DiskRefresh(name string) (supervisor.DiskUsage, error) {
	if s.disk == nil {
		return supervisor.DiskUsage{}, errors.New("disk usage is not enabled")
	}
	usage, err := s.disk.Refresh(name)
	if err != nil {
		return supervisor.DiskUsage{}, err
	}
	return diskUsage(usage), nil
}

func (s *Service) start(name string) error   { return s.runtime.Start(name) }
func (s *Service) stop(name string) error    { return s.runtime.Stop(name) }
func (s *Service) restart(name string) error { return s.runtime.Restart(name) }

func (s *Service) destroy(name string) error {
	if err := s.runtime.Destroy(name); err != nil {
		return err
	}
	if s.pubsub != nil {
		s.pubsub.Reconcile(s.runtime.Snapshots())
	}
	return nil
}

func (s *Service) maintenance(name string, on bool) error {
	return s.runtime.SetMaintenance(name, on)
}

func (s *Service) logs(name, process string, lines int) (map[string][]string, error) {
	return s.runtime.Logs(name, process, lines)
}

func (s *Service) ports() map[string]int { return s.runtime.Ports() }

// cron lists the scheduled jobs of one app from its live snapshot.
func (s *Service) cron(name string) ([]supervisor.CronSnapshot, error) {
	snapshot, err := s.runtime.Snapshot(name)
	if err != nil {
		return nil, err
	}
	return snapshot.Cron, nil
}

// runCron starts one scheduled job now.
func (s *Service) runCron(name, job string) error { return s.runtime.RunCron(name, job) }

// Hooks lists an app's deploy hooks with their effective secrets.
func (s *Service) Hooks(name string) ([]supervisor.HookInfo, error) { return s.runtime.Hooks(name) }

// runHook starts one deploy hook now.
func (s *Service) runHook(name, hook string) error { return s.runtime.RunHook(name, hook) }

// HostHookToken returns the token a ping to a host-level hook must present.
func (s *Service) HostHookToken(name string) (string, error) {
	return s.runtime.HostHookToken(name)
}

// DbossToken is tokens.dboss of the live host config: the credential of every hook ping and of
// /metrics. Empty means neither is served.
func (s *Service) DbossToken() string { return s.runtime.HostConfig().Tokens.Dboss }

// HostPages is the host's pages folder from the live host config.
func (s *Service) HostPages() string { return s.runtime.HostConfig().Pages }

// HookToken returns the token a ping to one app hook must present.
func (s *Service) HookToken(name, hook string) (string, error) {
	return s.runtime.HookToken(name, hook)
}

// exec runs one command in the app's environment and returns its combined output.
func (s *Service) exec(name string, argv []string, timeout time.Duration) (supervisor.ExecResult, error) {
	return s.runtime.Exec(name, argv, timeout)
}

// RestartRequired lists the host keys whose value on disk differs from the running session.
func (s *Service) RestartRequired() []string { return s.runtime.RestartRequired() }

// rescan reloads the apps and reports the invalid ones and the host keys waiting for a restart,
// so every transport answers with the same shape.
func (s *Service) rescan() (RescanResult, error) {
	invalid, err := s.runtime.Rescan()
	if s.pg != nil {
		// postgres is a host key that hot-reloads, so a rescan re-applies it like a config save.
		s.pg.Apply(s.runtime.HostConfig())
	}
	if s.pubsub != nil {
		s.pubsub.Reconcile(s.runtime.Snapshots())
	}
	result := RescanResult{Apps: s.Apps(), Invalid: messages(invalid), RestartRequired: s.runtime.RestartRequired()}
	return result, err
}

// decorate adds what the supervisor does not know about an app: how many requests it answered and
// what it occupies on disk. Both sources are optional and a missing one leaves the zero value.
func (s *Service) decorate(snapshot *supervisor.Snapshot) {
	if s.store != nil {
		s.withRates(snapshot)
	}
	if s.disk != nil {
		if usage, ok := s.disk.Usage(snapshot.Name); ok {
			snapshot.Disk = diskUsage(usage)
		}
	}
}

func (s *Service) withRates(snapshot *supervisor.Snapshot) {
	rates, err := s.store.Rates(snapshot.Name)
	if err != nil {
		return
	}
	snapshot.RequestRates = supervisor.RequestRates{LastMinute: rates.LastMinute, LastHour: rates.LastHour, LastDay: rates.LastDay}
}

func diskUsage(usage diskusage.Usage) supervisor.DiskUsage {
	return supervisor.DiskUsage{AppBytes: usage.AppBytes, LogBytes: usage.LogBytes, TotalBytes: usage.TotalBytes, MeasuredAt: usage.MeasuredAt}
}

func messages(failures []error) []string {
	result := make([]string, len(failures))
	for i, err := range failures {
		result[i] = err.Error()
	}
	return result
}
