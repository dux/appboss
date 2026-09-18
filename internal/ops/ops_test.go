package ops

import (
	"errors"
	"testing"

	"deploy-boss/internal/logstore"
	"deploy-boss/internal/super"
)

type fakeRuntime struct {
	snapshots []super.Snapshot
	actions   []string
	invalid   []error
	restart   []string
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
	return nil
}

func (f *fakeRuntime) Stop(name string) error {
	f.actions = append(f.actions, "stop "+name)
	return nil
}

func (f *fakeRuntime) Restart(name string) error {
	f.actions = append(f.actions, "restart "+name)
	return nil
}

func (f *fakeRuntime) SetMaintenance(name string, on bool) error {
	f.actions = append(f.actions, "maintenance "+name)
	return nil
}

func (f *fakeRuntime) Rescan() ([]error, error) {
	f.actions = append(f.actions, "rescan")
	return f.invalid, nil
}

func (f *fakeRuntime) RestartRequired() []string { return f.restart }

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
	service := New(runtime, nil, nil)
	cases := []struct {
		request Request
		action  string
	}{
		{Request{Method: ActionStart, App: "sinatra"}, "start sinatra"},
		{Request{Method: ActionStop, App: "sinatra"}, "stop sinatra"},
		{Request{Method: ActionRestart, App: "sinatra"}, "restart sinatra"},
		{Request{Method: ActionMaintenance, App: "sinatra", On: true}, "maintenance sinatra"},
		{Request{Method: ActionRescan}, "rescan"},
		{Request{Method: ActionLogs, App: "sinatra"}, "logs sinatra"},
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
	if _, err := New(&fakeRuntime{}, nil, nil).Do(Request{Method: "nope"}); !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("got %v, want ErrUnknownAction", err)
	}
}

func TestAppsAttachRequestRates(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []super.Snapshot{{Name: "sinatra"}, {Name: "bun"}}}
	service := New(runtime, fakeRates{"sinatra": {LastMinute: 2, LastHour: 7, LastDay: 20}}, nil)
	apps := service.Apps()
	if apps[0].RequestRates.LastHour != 7 {
		t.Fatalf("sinatra rates = %+v", apps[0].RequestRates)
	}
	if apps[1].RequestRates != (super.RequestRates{}) {
		t.Fatalf("bun should have no rates: %+v", apps[1].RequestRates)
	}
}

func TestRescanReportsInvalidAndRestartRequired(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []super.Snapshot{{Name: "sinatra"}}, invalid: []error{errors.New("bun: bad procfile")}, restart: []string{"proxy"}}
	result, err := New(runtime, nil, nil).Rescan()
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
