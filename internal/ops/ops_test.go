package ops

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/diskusage"
	"dboss/internal/logstore"
	"dboss/internal/pg"
	"dboss/internal/super"
)

type fakeRuntime struct {
	snapshots  []super.Snapshot
	actions    []string
	invalid    []error
	restart    []string
	startErr   error
	destroyErr error
}

func (f *fakeRuntime) Snapshots() []super.Snapshot {
	return append([]super.Snapshot(nil), f.snapshots...)
}

func (f *fakeRuntime) Snapshot(name string) (super.Snapshot, error) {
	for _, snapshot := range f.snapshots {
		if snapshot.Name == name {
			return snapshot, nil
		}
	}
	return super.Snapshot{}, errors.New("unknown app")
}

func (f *fakeRuntime) Start(name string) error {
	f.actions = append(f.actions, "start "+name)
	return f.startErr
}

func (f *fakeRuntime) Stop(name string) error {
	f.actions = append(f.actions, "stop "+name)
	return nil
}

func (f *fakeRuntime) Restart(name string) error {
	f.actions = append(f.actions, "restart "+name)
	return nil
}

func (f *fakeRuntime) Destroy(name string) error {
	f.actions = append(f.actions, "destroy "+name)
	return f.destroyErr
}

func (f *fakeRuntime) SetMaintenance(name string, on bool) error {
	f.actions = append(f.actions, "maintenance "+name)
	return nil
}

func (f *fakeRuntime) RunCron(name, job string) error {
	f.actions = append(f.actions, "cron-run "+name+"/"+job)
	return nil
}

func (f *fakeRuntime) RunHook(name, hook string) error {
	f.actions = append(f.actions, "hook-run "+name+"/"+hook)
	return nil
}

func (f *fakeRuntime) RotateHook(name, hook string) (super.HookInfo, error) {
	f.actions = append(f.actions, "hook-rotate "+name+"/"+hook)
	return super.HookInfo{HookSnapshot: super.HookSnapshot{Name: hook}, URL: "https://dboss.example.com/hooks/" + name + "/" + hook}, nil
}

func (f *fakeRuntime) Hooks(name string) ([]super.HookInfo, error) {
	f.actions = append(f.actions, "hook "+name)
	return []super.HookInfo{{HookSnapshot: super.HookSnapshot{Name: "deploy"}}}, nil
}

func (f *fakeRuntime) HookSecret(name, hook string) (string, error) {
	return "secret", nil
}

func (f *fakeRuntime) Exec(name string, argv []string, timeout time.Duration) (super.ExecResult, error) {
	f.actions = append(f.actions, "exec "+name)
	return super.ExecResult{Output: "ok", ExitCode: 0}, nil
}

func (f *fakeRuntime) Rescan() ([]error, error) {
	f.actions = append(f.actions, "rescan")
	return f.invalid, nil
}

func (f *fakeRuntime) RestartRequired() []string { return f.restart }
func (f *fakeRuntime) HostConfig() config.Config { return config.Default() }

func (f *fakeRuntime) Logs(name, process string, lines int) (map[string][]string, error) {
	f.actions = append(f.actions, "logs "+name)
	return map[string][]string{"web": {"line"}}, nil
}

func (f *fakeRuntime) Ports() map[string]int { return map[string]int{"web": 3100} }

type fakeRates map[string]logstore.Rates

func (r fakeRates) Rates(app string) (logstore.Rates, error) {
	rates, ok := r[app]
	if !ok {
		return logstore.Rates{}, errors.New("missing rate fixture")
	}
	return rates, nil
}

func TestDoRoutesToTheSameMethodForEveryTransport(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []super.Snapshot{{Name: "sinatra"}}}
	service := New(runtime, nil, nil, nil, nil, nil)
	cases := []struct {
		request Request
		action  string
	}{
		{Request{Method: ActionStart, App: "sinatra"}, "start sinatra"},
		{Request{Method: ActionStop, App: "sinatra"}, "stop sinatra"},
		{Request{Method: ActionRestart, App: "sinatra"}, "restart sinatra"},
		{Request{Method: ActionDestroy, App: "sinatra"}, "destroy sinatra"},
		{Request{Method: ActionMaintenance, App: "sinatra", On: true}, "maintenance sinatra"},
		{Request{Method: ActionRescan}, "rescan"},
		{Request{Method: ActionLogs, App: "sinatra"}, "logs sinatra"},
		{Request{Method: ActionCronRun, App: "sinatra", Job: "cleanup"}, "cron-run sinatra/cleanup"},
	}
	for _, item := range cases {
		runtime.actions = nil
		if _, err := service.Do(item.request); err != nil {
			t.Fatalf("%s: %v", item.request.Method, err)
		}
		if len(runtime.actions) != 1 || runtime.actions[0] != item.action {
			t.Fatalf("%s: ran %v, want %q", item.request.Method, runtime.actions, item.action)
		}
	}
}

func TestDoRejectsAnUnknownAction(t *testing.T) {
	if _, err := New(&fakeRuntime{}, nil, nil, nil, nil, nil).Do(Request{Method: "nope"}); !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("got %v, want ErrUnknownAction", err)
	}
}

func TestAppsAttachRequestRates(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []super.Snapshot{{Name: "sinatra"}, {Name: "bun"}}}
	service := New(runtime, fakeRates{"sinatra": {LastMinute: 2, LastHour: 7, LastDay: 20}}, nil, nil, nil, nil)
	apps := service.Apps()
	if apps[0].RequestRates.LastHour != 7 {
		t.Fatalf("sinatra rates = %+v", apps[0].RequestRates)
	}
	if apps[1].RequestRates != (super.RequestRates{}) {
		t.Fatalf("bun should have no rates: %+v", apps[1].RequestRates)
	}
}

// fakeDisk answers for one app only, so the test also covers an app the walk has not reached.
type fakeDisk map[string]diskusage.Usage

func (f fakeDisk) Usage(app string) (diskusage.Usage, bool) {
	usage, ok := f[app]
	return usage, ok
}

func (f fakeDisk) Refresh(app string) (diskusage.Usage, error) {
	usage, ok := f[app]
	if !ok {
		return diskusage.Usage{}, errors.New("unknown app")
	}
	return usage, nil
}

func TestAppsAttachDiskUsage(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []super.Snapshot{{Name: "sinatra"}, {Name: "bun"}}}
	measured := time.Now()
	service := New(runtime, nil, nil, nil, nil, fakeDisk{"sinatra": {AppBytes: 10, LogBytes: 5, TotalBytes: 15, MeasuredAt: measured}})
	apps := service.Apps()
	if apps[0].Disk.TotalBytes != 15 || apps[0].Disk.AppBytes != 10 || apps[0].Disk.LogBytes != 5 {
		t.Fatalf("sinatra disk = %+v", apps[0].Disk)
	}
	if !apps[1].Disk.MeasuredAt.IsZero() {
		t.Fatalf("bun has not been measured: %+v", apps[1].Disk)
	}
	one, err := service.App("sinatra")
	if err != nil || one.Disk.TotalBytes != 15 {
		t.Fatalf("App(sinatra) disk = %+v, err = %v", one.Disk, err)
	}
	usage, err := service.DiskRefresh("sinatra")
	if err != nil || usage.TotalBytes != 15 {
		t.Fatalf("DiskRefresh = %+v, err = %v", usage, err)
	}
	if _, err := service.DiskRefresh("bun"); err == nil {
		t.Fatal("refreshing an app the module does not know should fail")
	}
}

func TestAppsWithoutADiskModule(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []super.Snapshot{{Name: "sinatra"}}}
	service := New(runtime, nil, nil, nil, nil, nil)
	if apps := service.Apps(); len(apps) != 1 || apps[0].Disk != (super.DiskUsage{}) {
		t.Fatalf("apps = %+v", apps)
	}
	if _, err := service.DiskRefresh("sinatra"); err == nil {
		t.Fatal("a refresh without the module should fail")
	}
}

func TestCronReturnsScheduledJobs(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []super.Snapshot{{Name: "sinatra", Cron: []super.CronSnapshot{{Name: "cleanup"}}}}}
	jobs, err := New(runtime, nil, nil, nil, nil, nil).Cron("sinatra")
	if err != nil || len(jobs) != 1 || jobs[0].Name != "cleanup" {
		t.Fatalf("jobs = %+v, err = %v", jobs, err)
	}
}

func TestRescanReportsInvalidAndRestartRequired(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []super.Snapshot{{Name: "sinatra"}}, invalid: []error{errors.New("bun: bad procfile")}, restart: []string{"proxy"}}
	result, err := New(runtime, nil, nil, nil, nil, nil).Rescan()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Invalid) != 1 || result.Invalid[0] != "bun: bad procfile" || len(result.RestartRequired) != 1 || result.RestartRequired[0] != "proxy" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result.Apps) != 1 || result.Apps[0].Name != "sinatra" {
		t.Fatalf("rescan should return the fleet: %+v", result.Apps)
	}
}

func TestRescanAppliesPostgresConfig(t *testing.T) {
	postgres := &fakePG{}
	if _, err := New(&fakeRuntime{}, nil, nil, postgres, nil, nil).Rescan(); err != nil {
		t.Fatal(err)
	}
	if postgres.applied != 1 {
		t.Fatalf("rescan should re-apply the postgres config, applied=%d", postgres.applied)
	}
}

type fakePG struct {
	enabled   bool
	available bool
	backups   []pg.Backup
	applied   int
}

func (f *fakePG) Enabled() bool   { return f.enabled }
func (f *fakePG) Available() bool { return f.available }
func (f *fakePG) Snapshot() pg.Snapshot {
	return pg.Snapshot{Available: f.available}
}
func (f *fakePG) Refresh(context.Context) pg.Snapshot { return pg.Snapshot{Available: f.available} }
func (f *fakePG) BackupAll(context.Context) error     { return nil }
func (f *fakePG) BackupDatabase(context.Context, string, bool) (pg.Backup, error) {
	return pg.Backup{Database: "app", Status: "ok"}, nil
}
func (f *fakePG) Backups() []pg.Backup { return f.backups }
func (f *fakePG) BackupFile(id string) (pg.Backup, string, error) {
	for _, entry := range f.backups {
		if entry.ID == id {
			return entry, "/tmp/" + entry.ID, nil
		}
	}
	return pg.Backup{}, "", errors.New("unknown backup")
}
func (f *fakePG) ImportBackup(database string, source io.Reader) (pg.Backup, error) {
	data, err := io.ReadAll(source)
	if err != nil {
		return pg.Backup{}, err
	}
	entry := pg.Backup{ID: "uploaded", Database: database, Status: "ok", Manual: true, Bytes: int64(len(data))}
	f.backups = append(f.backups, entry)
	return entry, nil
}
func (f *fakePG) DeleteBackup(string) error { return nil }
func (f *fakePG) Restore(context.Context, pg.RestoreRequest) (pg.RestoreResult, error) {
	return pg.RestoreResult{Target: "app_restore"}, nil
}
func (f *fakePG) DropDatabase(context.Context, string, string) error { return nil }
func (f *fakePG) BackupConfig() config.PostgresBackup                { return config.PostgresBackup{} }
func (f *fakePG) Apply(config.Config)                                { f.applied++ }

func TestPGActionsDispatch(t *testing.T) {
	postgres := &fakePG{enabled: true, available: true, backups: []pg.Backup{{ID: "b1", Database: "app"}}}
	service := New(&fakeRuntime{}, nil, nil, postgres, nil, nil)

	if !service.PGAvailable() {
		t.Fatal("PGAvailable should be true")
	}
	if _, err := service.Do(Request{Method: ActionPGBackup, Database: "app"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Do(Request{Method: ActionPGRestore, BackupID: "b1", Target: "app_restore"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Do(Request{Method: ActionPG}); err != nil {
		t.Fatal(err)
	}
	if entries := service.Backups(); len(entries) != 1 {
		t.Fatalf("backups = %+v", entries)
	}
	service.ApplyPGConfig(config.Default())
	if postgres.applied != 1 {
		t.Fatalf("apply count = %d", postgres.applied)
	}
}

func TestPGActionsDisabledWithoutService(t *testing.T) {
	service := New(&fakeRuntime{}, nil, nil, nil, nil, nil)
	if service.PGAvailable() {
		t.Fatal("PGAvailable should be false without a service")
	}
	if _, err := service.Do(Request{Method: ActionPG}); err == nil {
		t.Fatal("pg action should fail without a service")
	}
}

// fakeAuditStore implements both LogStore and Auditor, which is what makes ops.New turn auditing on.
type fakeAuditStore struct {
	audits []logstore.AuditEntry
}

func (s *fakeAuditStore) SearchLogs(string, logstore.LogFilter) ([]logstore.LogEntry, error) {
	return nil, nil
}
func (s *fakeAuditStore) SearchRequests(string, logstore.RequestFilter) ([]logstore.RequestEntry, error) {
	return nil, nil
}
func (s *fakeAuditStore) Channels(string) ([]logstore.Channel, error) { return nil, nil }
func (s *fakeAuditStore) Tree([]string) ([]logstore.AppTree, error)   { return nil, nil }
func (s *fakeAuditStore) RecordAudit(entry logstore.AuditEntry) error {
	s.audits = append(s.audits, entry)
	return nil
}
func (s *fakeAuditStore) SearchAudit(logstore.AuditFilter) ([]logstore.AuditEntry, error) {
	return s.audits, nil
}

func TestAuditedActionsRecordTheActorAndResult(t *testing.T) {
	store := &fakeAuditStore{}
	service := New(&fakeRuntime{}, nil, store, nil, nil, nil)

	if _, err := service.Do(Request{Method: ActionStart, App: "sinatra"}); err != nil {
		t.Fatal(err)
	}
	entry := store.audits[0]
	if entry.Actor != "cli" || entry.Action != ActionStart || entry.Result != "ok" {
		t.Fatalf("audit = %+v", entry)
	}

	if _, err := service.Do(Request{Method: ActionStop, App: "sinatra", Actor: "bob"}); err != nil {
		t.Fatal(err)
	}
	if got := store.audits[1].Actor; got != "bob" {
		t.Fatalf("explicit actor = %q", got)
	}

	if _, err := service.Do(Request{Method: ActionDestroy, App: "sinatra", Actor: "bob"}); err != nil {
		t.Fatal(err)
	}
	entry = store.audits[2]
	if entry.Actor != "bob" || entry.Action != ActionDestroy || entry.Result != "ok" {
		t.Fatalf("destroy audit = %+v", entry)
	}

	failing := New(&fakeRuntime{startErr: errors.New("port busy")}, nil, store, nil, nil, nil)
	if _, err := failing.Do(Request{Method: ActionStart, App: "sinatra"}); err == nil {
		t.Fatal("expected start error")
	}
	last := store.audits[len(store.audits)-1]
	if last.Result != "error" || last.Error != "port busy" {
		t.Fatalf("failed audit = %+v", last)
	}

	failing = New(&fakeRuntime{destroyErr: errors.New("not deletable")}, nil, store, nil, nil, nil)
	if _, err := failing.Do(Request{Method: ActionDestroy, App: "sinatra"}); err == nil {
		t.Fatal("expected destroy error")
	}
	last = store.audits[len(store.audits)-1]
	if last.Action != ActionDestroy || last.Result != "error" || last.Error != "not deletable" {
		t.Fatalf("failed destroy audit = %+v", last)
	}
}

func TestNonAuditedActionsWriteNoRow(t *testing.T) {
	store := &fakeAuditStore{}
	if _, err := New(&fakeRuntime{}, nil, store, nil, nil, nil).Do(Request{Method: ActionList}); err != nil {
		t.Fatal(err)
	}
	if len(store.audits) != 0 {
		t.Fatalf("ls must not audit: %+v", store.audits)
	}
}
