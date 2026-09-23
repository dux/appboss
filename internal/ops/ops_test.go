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
	"dboss/internal/supervisor"
)

type fakeRuntime struct {
	snapshots  []supervisor.Snapshot
	actions    []string
	invalid    []error
	restart    []string
	startErr   error
	destroyErr error
}

func (f *fakeRuntime) Snapshots() []supervisor.Snapshot {
	return append([]supervisor.Snapshot(nil), f.snapshots...)
}

func (f *fakeRuntime) Snapshot(name string) (supervisor.Snapshot, error) {
	for _, snapshot := range f.snapshots {
		if snapshot.Name == name {
			return snapshot, nil
		}
	}
	return supervisor.Snapshot{}, errors.New("unknown app")
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

func (f *fakeRuntime) StartProcess(name, process string) error {
	f.actions = append(f.actions, "start "+name+"/"+process)
	return nil
}

func (f *fakeRuntime) StopProcess(name, process string) error {
	f.actions = append(f.actions, "stop "+name+"/"+process)
	return nil
}

func (f *fakeRuntime) RestartProcess(name, process string) error {
	f.actions = append(f.actions, "restart "+name+"/"+process)
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

func (f *fakeRuntime) Hooks(name string) ([]supervisor.HookInfo, error) {
	f.actions = append(f.actions, "hook "+name)
	return []supervisor.HookInfo{{HookSnapshot: supervisor.HookSnapshot{Name: "deploy"}}}, nil
}

func (f *fakeRuntime) HookToken(name, hook string) (string, error) {
	return "secret", nil
}

func (f *fakeRuntime) HostHookToken(name string) (string, error) {
	return "secret", nil
}

func (f *fakeRuntime) Exec(name string, argv []string, timeout time.Duration) (supervisor.ExecResult, error) {
	f.actions = append(f.actions, "exec "+name)
	return supervisor.ExecResult{Output: "ok", ExitCode: 0}, nil
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

// fakeRates answers only the request counters; any other store call panics on the nil LogStore.
type fakeRates struct {
	LogStore
	rates map[string]logstore.Rates
}

func (r fakeRates) Rates(app string) (logstore.Rates, error) {
	rates, ok := r.rates[app]
	if !ok {
		return logstore.Rates{}, errors.New("missing rate fixture")
	}
	return rates, nil
}

func TestDoRoutesToTheSameMethodForEveryTransport(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []supervisor.Snapshot{{Name: "sinatra"}}}
	service := New(runtime, nil, nil, nil, nil, nil)
	cases := []struct {
		request Request
		action  string
	}{
		{Request{Method: ActionStart, App: "sinatra"}, "start sinatra"},
		{Request{Method: ActionStop, App: "sinatra"}, "stop sinatra"},
		{Request{Method: ActionRestart, App: "sinatra"}, "restart sinatra"},
		{Request{Method: ActionStart, App: "sinatra", Process: "job"}, "start sinatra/job"},
		{Request{Method: ActionStop, App: "sinatra", Process: "job"}, "stop sinatra/job"},
		{Request{Method: ActionRestart, App: "sinatra", Process: "job"}, "restart sinatra/job"},
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
	runtime := &fakeRuntime{snapshots: []supervisor.Snapshot{{Name: "sinatra"}, {Name: "bun"}}}
	service := New(runtime, fakeRates{rates: map[string]logstore.Rates{"sinatra": {LastMinute: 2, LastHour: 7, LastDay: 20}}}, nil, nil, nil, nil)
	apps := service.Apps()
	if apps[0].RequestRates.LastHour != 7 {
		t.Fatalf("sinatra rates = %+v", apps[0].RequestRates)
	}
	if apps[1].RequestRates != (supervisor.RequestRates{}) {
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
	runtime := &fakeRuntime{snapshots: []supervisor.Snapshot{{Name: "sinatra"}, {Name: "bun"}}}
	measured := time.Now()
	service := New(runtime, nil, nil, nil, fakeDisk{"sinatra": {AppBytes: 10, LogBytes: 5, TotalBytes: 15, MeasuredAt: measured}}, nil)
	apps := service.Apps()
	if apps[0].Disk.TotalBytes != 15 || apps[0].Disk.AppBytes != 10 || apps[0].Disk.LogBytes != 5 {
		t.Fatalf("sinatra disk = %+v", apps[0].Disk)
	}
	if !apps[1].Disk.MeasuredAt.IsZero() {
		t.Fatalf("bun has not been measured: %+v", apps[1].Disk)
	}
	one, err := service.app("sinatra")
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
	runtime := &fakeRuntime{snapshots: []supervisor.Snapshot{{Name: "sinatra"}}}
	service := New(runtime, nil, nil, nil, nil, nil)
	if apps := service.Apps(); len(apps) != 1 || apps[0].Disk != (supervisor.DiskUsage{}) {
		t.Fatalf("apps = %+v", apps)
	}
	if _, err := service.DiskRefresh("sinatra"); err == nil {
		t.Fatal("a refresh without the module should fail")
	}
}

func TestCronReturnsScheduledJobs(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []supervisor.Snapshot{{Name: "sinatra", Cron: []supervisor.CronSnapshot{{Name: "cleanup"}}}}}
	jobs, err := New(runtime, nil, nil, nil, nil, nil).cron("sinatra")
	if err != nil || len(jobs) != 1 || jobs[0].Name != "cleanup" {
		t.Fatalf("jobs = %+v, err = %v", jobs, err)
	}
}

func TestRescanReportsInvalidAndRestartRequired(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []supervisor.Snapshot{{Name: "sinatra"}}, invalid: []error{errors.New("bun: bad procfile")}, restart: []string{"proxy"}}
	result, err := New(runtime, nil, nil, nil, nil, nil).rescan()
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
	if _, err := New(&fakeRuntime{}, nil, postgres, nil, nil, nil).rescan(); err != nil {
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
	queried   string
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
func (f *fakePG) Query(_ context.Context, database, sql string) (pg.QueryResult, error) {
	f.queried = sql
	return pg.QueryResult{Database: database, Columns: []string{"one"}, Rows: [][]any{{"1"}}, RowCount: 1, Command: "SELECT 1"}, nil
}
func (f *fakePG) BackupConfig() config.PostgresBackups { return config.PostgresBackups{} }
func (f *fakePG) Apply(config.Config)                  { f.applied++ }

func TestPGActionsDispatch(t *testing.T) {
	postgres := &fakePG{enabled: true, available: true, backups: []pg.Backup{{ID: "b1", Database: "app"}}}
	service := New(&fakeRuntime{}, nil, postgres, nil, nil, nil)

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
	if _, err := service.Do(Request{Method: ActionPGQuery, Database: "app", SQL: "select 1"}); err != nil {
		t.Fatal(err)
	}
	if postgres.queried != "select 1" {
		t.Fatalf("query = %q", postgres.queried)
	}
	if entries := service.Backups(); len(entries) != 1 {
		t.Fatalf("backups = %+v", entries)
	}
	// postgres hot-reloads: a rescan re-applies the host block.
	if _, err := service.rescan(); err != nil {
		t.Fatal(err)
	}
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

func TestAuditedActionsRecordTheActorAndResult(t *testing.T) {
	store := &auditStore{}
	service := New(&fakeRuntime{}, store, nil, nil, nil, nil)

	if _, err := service.Do(Request{Method: ActionStart, App: "sinatra"}); err != nil {
		t.Fatal(err)
	}
	entry := store.rows[0]
	if entry.Actor != "cli" || entry.Action != ActionStart || entry.Result != "ok" {
		t.Fatalf("audit = %+v", entry)
	}

	if _, err := service.Do(Request{Method: ActionStop, App: "sinatra", Actor: "bob"}); err != nil {
		t.Fatal(err)
	}
	if got := store.rows[1].Actor; got != "bob" {
		t.Fatalf("explicit actor = %q", got)
	}

	if _, err := service.Do(Request{Method: ActionDestroy, App: "sinatra", Actor: "bob"}); err != nil {
		t.Fatal(err)
	}
	entry = store.rows[2]
	if entry.Actor != "bob" || entry.Action != ActionDestroy || entry.Result != "ok" {
		t.Fatalf("destroy audit = %+v", entry)
	}
	if _, err := service.Do(Request{Method: ActionRestart, App: "sinatra", Process: "job"}); err != nil {
		t.Fatal(err)
	}
	if got := store.rows[3].Detail; got != "job" {
		t.Fatalf("process detail = %q", got)
	}

	failing := New(&fakeRuntime{startErr: errors.New("port busy")}, store, nil, nil, nil, nil)
	if _, err := failing.Do(Request{Method: ActionStart, App: "sinatra"}); err == nil {
		t.Fatal("expected start error")
	}
	last := store.rows[len(store.rows)-1]
	if last.Result != "error" || last.Error != "port busy" {
		t.Fatalf("failed audit = %+v", last)
	}

	failing = New(&fakeRuntime{destroyErr: errors.New("not deletable")}, store, nil, nil, nil, nil)
	if _, err := failing.Do(Request{Method: ActionDestroy, App: "sinatra"}); err == nil {
		t.Fatal("expected destroy error")
	}
	last = store.rows[len(store.rows)-1]
	if last.Action != ActionDestroy || last.Result != "error" || last.Error != "not deletable" {
		t.Fatalf("failed destroy audit = %+v", last)
	}
}

func TestNonAuditedActionsWriteNoRow(t *testing.T) {
	store := &auditStore{}
	if _, err := New(&fakeRuntime{}, store, nil, nil, nil, nil).Do(Request{Method: ActionList}); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 0 {
		t.Fatalf("ls must not audit: %+v", store.rows)
	}
}
