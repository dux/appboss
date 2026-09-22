package ctl

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/logstore"
	"dboss/internal/ops"
	"dboss/internal/super"
)

// fakeRuntime satisfies ops.Runtime with the bare minimum the control-socket tests exercise.
type fakeRuntime struct {
	started []string
}

func (f *fakeRuntime) Snapshots() []super.Snapshot { return []super.Snapshot{{Name: "alpha"}} }
func (f *fakeRuntime) Snapshot(string) (super.Snapshot, error) {
	return super.Snapshot{}, errors.New("unknown app")
}
func (f *fakeRuntime) Start(name string) error { f.started = append(f.started, name); return nil }
func (f *fakeRuntime) Stop(string) error       { return nil }
func (f *fakeRuntime) Restart(string) error    { return nil }
func (f *fakeRuntime) Destroy(string) error    { return nil }
func (f *fakeRuntime) SetMaintenance(string, bool) error {
	return nil
}
func (f *fakeRuntime) RunCron(string, string) error { return nil }
func (f *fakeRuntime) RunHook(string, string) error { return nil }
func (f *fakeRuntime) RotateHook(name, hook string) (super.HookInfo, error) {
	return super.HookInfo{HookSnapshot: super.HookSnapshot{Name: hook}}, nil
}
func (f *fakeRuntime) Hooks(string) ([]super.HookInfo, error) { return nil, nil }
func (f *fakeRuntime) HookSecret(string, string) (string, error) {
	return "", nil
}
func (f *fakeRuntime) HostHookSecret(string) (string, error) { return "", nil }
func (f *fakeRuntime) Exec(string, []string, time.Duration) (super.ExecResult, error) {
	return super.ExecResult{}, nil
}
func (f *fakeRuntime) Rescan() ([]error, error) { return nil, nil }
func (f *fakeRuntime) RestartRequired() []string {
	return nil
}
func (f *fakeRuntime) HostConfig() config.Config                             { return config.Default() }
func (f *fakeRuntime) Logs(string, string, int) (map[string][]string, error) { return nil, nil }
func (f *fakeRuntime) Ports() map[string]int                                 { return nil }

// auditStore implements both the read side (ops.LogStore) and ops.Auditor so a test can see the
// actor a transport attributed an action to.
type auditStore struct {
	audits []logstore.AuditEntry
}

func (s *auditStore) SearchLogs(string, logstore.LogFilter) ([]logstore.LogEntry, error) {
	return nil, nil
}
func (s *auditStore) SearchRequests(string, logstore.RequestFilter) ([]logstore.RequestEntry, error) {
	return nil, nil
}
func (s *auditStore) Channels(string) ([]logstore.Channel, error) { return nil, nil }
func (s *auditStore) Tree([]string) ([]logstore.AppTree, error)   { return nil, nil }
func (s *auditStore) RecordAudit(entry logstore.AuditEntry) error {
	s.audits = append(s.audits, entry)
	return nil
}
func (s *auditStore) SearchAudit(logstore.AuditFilter) ([]logstore.AuditEntry, error) {
	return s.audits, nil
}

func TestDispatchAttributesActionsToCLI(t *testing.T) {
	runtime := &fakeRuntime{}
	store := &auditStore{}
	server := &Server{service: ops.New(runtime, nil, store, nil, nil, nil)}

	if response := server.dispatch(Request{Method: ops.ActionStart, App: "alpha"}); !response.OK {
		t.Fatalf("start failed: %s", response.Error)
	}
	if len(runtime.started) != 1 || runtime.started[0] != "alpha" {
		t.Fatalf("started = %v", runtime.started)
	}
	if len(store.audits) != 1 || store.audits[0].Actor != "cli" {
		t.Fatalf("audit = %v, want one row attributed to cli", store.audits)
	}

	if response := server.dispatch(Request{Method: ops.ActionStop, App: "alpha", Actor: "bob"}); !response.OK {
		t.Fatalf("stop failed: %s", response.Error)
	}
	if last := store.audits[len(store.audits)-1]; last.Actor != "bob" {
		t.Fatalf("explicit actor = %q, want bob", last.Actor)
	}
}

func TestDispatchReturnsActionErrors(t *testing.T) {
	server := &Server{service: ops.New(&fakeRuntime{}, nil, nil, nil, nil, nil)}
	response := server.dispatch(Request{Method: "nope"})
	if response.OK || response.Error == "" {
		t.Fatalf("unknown action response = %+v", response)
	}
}

func TestLoginResponse(t *testing.T) {
	withoutLogin := &Server{service: ops.New(&fakeRuntime{}, nil, nil, nil, nil, nil)}
	if response := withoutLogin.dispatch(Request{Method: loginMethod}); response.OK || response.Error == "" {
		t.Fatalf("login without handler = %+v", response)
	}

	withLogin := &Server{
		service: ops.New(&fakeRuntime{}, nil, nil, nil, nil, nil),
		login:   func() (string, string, error) { return "http://local", "https://public", nil },
	}
	response := withLogin.dispatch(Request{Method: loginMethod})
	if !response.OK {
		t.Fatalf("login failed: %s", response.Error)
	}
	data, ok := response.Data.(map[string]string)
	if !ok || data["url"] != "http://local" || data["public_url"] != "https://public" {
		t.Fatalf("login data = %#v", response.Data)
	}
}

func TestListenServesAndRefusesASecondServer(t *testing.T) {
	// A unix socket path is capped near 104 bytes on macOS, so keep it short.
	dir, err := os.MkdirTemp("/tmp", "dboss-ctl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "dboss.sock")
	service := ops.New(&fakeRuntime{}, nil, nil, nil, nil, nil)
	server, listenErr := Listen(socket, service, nil)
	if listenErr != nil {
		t.Fatal(listenErr)
	}
	defer server.Close()

	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode = %o, want 660", info.Mode().Perm())
	}

	if _, err := Listen(socket, service, nil); err == nil {
		t.Fatal("second Listen on an active socket should fail")
	}

	var apps []super.Snapshot
	if err := (Client{Socket: socket, Timeout: time.Second}).Call(Request{Method: ops.ActionList}, &apps); err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].Name != "alpha" {
		t.Fatalf("apps = %v", apps)
	}

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("socket still present after Close: %v", err)
	}
}
