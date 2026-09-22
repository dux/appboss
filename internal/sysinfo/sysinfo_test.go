package sysinfo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProbeRecordsVersionAndPath(t *testing.T) {
	inspector := NewInspector(nil)
	inspector.lookPath = func(string) (string, error) { return "/usr/bin/node", nil }
	inspector.run = func(context.Context, string, ...string) (string, error) { return "v20.1.0\n", nil }
	tool := inspector.probe(context.Background(), probe{name: "node", args: []string{"--version"}, home: "https://nodejs.org"})
	if !tool.Found || tool.Path != "/usr/bin/node" || tool.Version != "v20.1.0" || tool.Home != "https://nodejs.org" || tool.Error != "" {
		t.Fatalf("unexpected tool: %+v", tool)
	}
}

// lsof -v prints its version and exits non-zero; the text is still the version.
func TestProbeKeepsVersionOnNonZeroExit(t *testing.T) {
	inspector := NewInspector(nil)
	inspector.lookPath = func(string) (string, error) { return "/usr/sbin/lsof", nil }
	inspector.run = func(context.Context, string, ...string) (string, error) {
		return "lsof version 4.99\n", errors.New("exit status 1")
	}
	tool := inspector.probe(context.Background(), probe{name: "lsof", args: []string{"-v"}})
	if tool.Version != "lsof version 4.99" || tool.Error != "" {
		t.Fatalf("unexpected tool: %+v", tool)
	}
}

func TestProbeMissingToolIsData(t *testing.T) {
	inspector := NewInspector(nil)
	inspector.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	inspector.run = func(context.Context, string, ...string) (string, error) { return "", nil }
	tool := inspector.probe(context.Background(), probe{name: "ghost"})
	if tool.Found || tool.Path != "" || tool.Error != "" {
		t.Fatalf("missing tool should be a plain not-found: %+v", tool)
	}
}

func TestFirstLineSkipsBlanksAndTrims(t *testing.T) {
	if got := firstLine("\n\n  hello world  \nsecond"); got != "hello world" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine("   \n"); got != "" {
		t.Fatalf("blank output should be empty, got %q", got)
	}
}

// The tool list is expensive to probe, so a refresh inside the interval reuses it.
func TestRefreshReusesToolsUntilInterval(t *testing.T) {
	inspector := NewInspector(nil)
	inspector.probes = []probe{{name: "alpha"}, {name: "beta"}}
	inspector.release, inspector.echo = nil, nil
	inspector.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	probesRun := 0
	inspector.run = func(context.Context, string, ...string) (string, error) {
		probesRun++
		return "1.0", nil
	}
	inspector.Refresh(context.Background())
	inspector.Refresh(context.Background())
	if probesRun != 2 {
		t.Fatalf("tools probed %d times, want 2 on the first refresh only", probesRun)
	}
	inspector.mu.Lock()
	inspector.lastTools = time.Now().Add(-inspector.toolInterval - time.Second)
	inspector.mu.Unlock()
	inspector.Refresh(context.Background())
	if probesRun != 4 {
		t.Fatalf("tools probed %d times, want 4 after the interval", probesRun)
	}
}

// The latest release leaves the box, so it is asked for once per interval and a failed lookup
// backs off the same way rather than stalling every refresh.
func TestRefreshCachesLatestRelease(t *testing.T) {
	inspector := NewInspector(nil)
	inspector.probes = nil
	inspector.echo = nil
	lookups, tag, lookupErr := 0, "v84", error(nil)
	inspector.release = newRemote(func(context.Context) (string, error) {
		lookups++
		return tag, lookupErr
	})

	snapshot := inspector.Refresh(context.Background())
	if snapshot.Runtime.DbossLatest != "v84" || snapshot.Runtime.DbossLatestURL == "" {
		t.Fatalf("unexpected runtime: %+v", snapshot.Runtime)
	}
	inspector.Refresh(context.Background())
	if lookups != 1 {
		t.Fatalf("release looked up %d times, want 1 inside the interval", lookups)
	}

	tag, lookupErr = "", errors.New("no route to host")
	inspector.release.last = time.Now().Add(-inspector.release.interval - time.Second)
	snapshot = inspector.Refresh(context.Background())
	if snapshot.Runtime.DbossLatest != "" || snapshot.Runtime.DbossLatestURL != "" {
		t.Fatalf("a failed lookup should leave the fields empty: %+v", snapshot.Runtime)
	}
	inspector.Refresh(context.Background())
	if lookups != 2 {
		t.Fatalf("release looked up %d times, want 2 after one failure", lookups)
	}
}

func TestRefreshCollectsHostAndDirs(t *testing.T) {
	dir := t.TempDir()
	inspector := NewInspector([]DirSpec{{Name: "state", Path: dir}})
	inspector.probes = nil
	inspector.release, inspector.echo = nil, nil
	snapshot := inspector.Refresh(context.Background())
	if snapshot.CollectedAt.IsZero() {
		t.Fatal("snapshot has no collection time")
	}
	if snapshot.Host.Hostname == "" || snapshot.Host.CPUs == 0 {
		t.Fatalf("host facts are empty: %+v", snapshot.Host)
	}
	if len(snapshot.Dirs) != 1 || snapshot.Dirs[0].TotalBytes <= 0 || snapshot.Dirs[0].Error != "" {
		t.Fatalf("unexpected dirs: %+v", snapshot.Dirs)
	}
}

func TestCollectDirsReportsMissingPath(t *testing.T) {
	inspector := NewInspector([]DirSpec{{Name: "gone", Path: "/dboss/does/not/exist"}})
	dirs := collectDirs(inspector.dirs)
	if len(dirs) != 1 || dirs[0].Error == "" {
		t.Fatalf("missing path should carry an error: %+v", dirs)
	}
}

// A zero load is a real value; the Sys tab formats all three unconditionally.
func TestHostJSONKeepsZeroLoad(t *testing.T) {
	data, err := json.Marshal(Host{Load1: 0.4})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"load1":`, `"load5":`, `"load15":`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("missing %s in %s", key, data)
		}
	}
}
