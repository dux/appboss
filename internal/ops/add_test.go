package ops

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/config"
	"dboss/internal/git"
	"dboss/internal/supervisor"
)

// sourceRepo commits files into a fresh repository and returns its path.
func sourceRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := filepath.Join(t.TempDir(), "shop")
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	for name, contents := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

// addRuntime is a host whose rescan loads every app folder that parses, like discovery does.
func addRuntime(t *testing.T, existing ...supervisor.Snapshot) *fakeRuntime {
	t.Helper()
	original := normalizeRepo
	normalizeRepo = func(input string) (string, string, error) {
		name, err := git.RepoName(input)
		return input, name, err
	}
	t.Cleanup(func() { normalizeRepo = original })

	cfg := config.Default()
	cfg.Apps = t.TempDir()
	return &fakeRuntime{host: &cfg, snapshots: existing, onRescan: func(f *fakeRuntime) {
		f.snapshots = append([]supervisor.Snapshot(nil), existing...)
		entries, _ := os.ReadDir(cfg.Apps)
		for _, entry := range entries {
			path, err := config.FindInDir(filepath.Join(cfg.Apps, entry.Name()))
			if err != nil {
				continue
			}
			app, err := config.LoadApp(path, cfg.Defaults)
			if err != nil {
				continue
			}
			f.snapshots = append(f.snapshots, supervisor.Snapshot{Name: entry.Name(), Hosts: app.Hosts})
		}
	}}
}

const webApp = "procfile:\n  web:\n    command: ./server\n    hosts: [shop.example.com]\n    canonical_host: shop.example.com\n"

func TestAddClonesLoadsAndStarts(t *testing.T) {
	source := sourceRepo(t, map[string]string{"dboss.yaml": webApp})
	runtime := addRuntime(t)
	result, err := New(runtime, nil, nil, nil, nil, nil).Do(Request{Method: ActionAdd, Repo: source})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := result.(supervisor.Snapshot); snapshot.Name != "shop" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if got := strings.Join(runtime.actions, ","); got != "rescan,start shop" {
		t.Fatalf("actions = %s", got)
	}
	if _, err := os.Stat(filepath.Join(runtime.host.Apps, "shop", config.LocalFileName)); !os.IsNotExist(err) {
		t.Fatalf("no host given, but a local file exists: %v", err)
	}
}

func TestAddHostOverrideWritesLocalFile(t *testing.T) {
	source := sourceRepo(t, map[string]string{"config/dboss.yaml": webApp})
	runtime := addRuntime(t)
	if _, err := New(runtime, nil, nil, nil, nil, nil).Do(Request{Method: ActionAdd, Repo: source, App: "store", Host: "Store.Box.Test"}); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(runtime.host.Apps, "store", "config", config.LocalFileName)
	app, err := config.LoadApp(local, runtime.host.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Hosts) != 1 || app.Hosts[0] != "store.box.test" || app.WebProcesses[0].CanonicalHost != "" {
		t.Fatalf("hosts = %v canonical = %q", app.Hosts, app.WebProcesses[0].CanonicalHost)
	}
}

func TestAddRefusesAndCleansUp(t *testing.T) {
	twoWebs := "procfile:\n  web:\n    command: ./a\n    hosts: [a.test]\n  api:\n    command: ./b\n    hosts: [b.test]\n"
	cases := []struct {
		name    string
		files   map[string]string
		request Request
		running []supervisor.Snapshot
		want    string
	}{
		{"no config", map[string]string{"README": "x"}, Request{}, nil, "not a dboss app"},
		{"invalid config", map[string]string{"dboss.yaml": "procfile: {}\n"}, Request{}, nil, "procfile"},
		{"host clash", map[string]string{"dboss.yaml": webApp}, Request{}, []supervisor.Snapshot{{Name: "old", Hosts: []string{"SHOP.example.com"}}}, "already served by app old"},
		{"override needs one web", map[string]string{"dboss.yaml": twoWebs}, Request{Host: "x.test"}, nil, "exactly one web process"},
		{"invalid name", map[string]string{"dboss.yaml": webApp}, Request{App: "Bad Name"}, nil, "invalid app name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime := addRuntime(t, tc.running...)
			request := tc.request
			request.Method, request.Repo = ActionAdd, sourceRepo(t, tc.files)
			_, err := New(runtime, nil, nil, nil, nil, nil).Do(request)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if entries, _ := os.ReadDir(runtime.host.Apps); len(entries) != 0 {
				t.Fatalf("apps folder kept %d entries", len(entries))
			}
			for _, action := range runtime.actions {
				if strings.HasPrefix(action, "start") || strings.HasPrefix(action, "stop") {
					t.Fatalf("unexpected action %s", action)
				}
			}
		})
	}
}

func TestAddRefusesAnExistingApp(t *testing.T) {
	source := sourceRepo(t, map[string]string{"dboss.yaml": webApp})
	runtime := addRuntime(t)
	existing := filepath.Join(runtime.host.Apps, "shop")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := New(runtime, nil, nil, nil, nil, nil).Do(Request{Method: ActionAdd, Repo: source})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(existing); err != nil {
		t.Fatalf("existing folder was removed: %v", err)
	}
}

func TestAddAuditsTheDerivedName(t *testing.T) {
	store := &auditStore{}
	runtime := addRuntime(t)
	source := sourceRepo(t, map[string]string{"dboss.yaml": webApp})
	if _, err := New(runtime, store, nil, nil, nil, nil).Do(Request{Method: ActionAdd, Repo: source, Branch: "no-such-branch", Actor: "bob"}); err == nil {
		t.Fatal("expected the missing branch to fail the clone")
	}
	entry := store.rows[0]
	if entry.App != "shop" || entry.Action != ActionAdd || entry.Result != "error" || entry.Detail != source+" branch=no-such-branch" {
		t.Fatalf("audit = %+v", entry)
	}
}
