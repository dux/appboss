package tmpclean

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dboss/internal/super"
)

// write creates path with the given age, making the parent directories as it goes.
func write(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func age(t *testing.T, path string, age time.Duration) {
	t.Helper()
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestSweepRemovesOldFilesAndEmptyDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tmp")
	write(t, filepath.Join(root, "old.cache"), 30*24*time.Hour)
	write(t, filepath.Join(root, "fresh.cache"), time.Hour)
	write(t, filepath.Join(root, "cache", "deep", "old.bin"), 30*24*time.Hour)
	age(t, filepath.Join(root, "cache", "deep"), 30*24*time.Hour)
	age(t, filepath.Join(root, "cache"), 30*24*time.Hour)

	removed, err := Sweep(root, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "old.cache")); !os.IsNotExist(err) {
		t.Fatal("old file should be gone")
	}
	if _, err := os.Stat(filepath.Join(root, "fresh.cache")); err != nil {
		t.Fatalf("fresh file should stay: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "cache")); !os.IsNotExist(err) {
		t.Fatal("emptied directory should be gone")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("tmp itself should stay: %v", err)
	}
}

func TestSweepKeepsFreshAndNonEmptyDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tmp")
	write(t, filepath.Join(root, "pids", "server.pid"), time.Minute)
	age(t, filepath.Join(root, "pids"), 30*24*time.Hour)
	if err := os.MkdirAll(filepath.Join(root, "sockets"), 0o755); err != nil {
		t.Fatal(err)
	}

	removed, err := Sweep(root, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "pids", "server.pid")); err != nil {
		t.Fatalf("fresh pid file should stay: %v", err)
	}
	// An empty directory younger than the cutoff is left for the app to use.
	if _, err := os.Stat(filepath.Join(root, "sockets")); err != nil {
		t.Fatalf("fresh empty directory should stay: %v", err)
	}
}

func TestSweepFollowsASymlinkedTmp(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared", "tmp")
	write(t, filepath.Join(shared, "old.cache"), 30*24*time.Hour)
	release := filepath.Join(dir, "release")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(release, "tmp")); err != nil {
		t.Fatal(err)
	}

	removed, err := Sweep(filepath.Join(release, "tmp"), time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Lstat(filepath.Join(release, "tmp")); err != nil {
		t.Fatalf("the symlink itself should stay: %v", err)
	}
}

func TestSweepMissingDirectoryIsNoError(t *testing.T) {
	removed, err := Sweep(filepath.Join(t.TempDir(), "tmp"), time.Now())
	if err != nil || removed != 0 {
		t.Fatalf("sweep of a missing tmp = %d, %v", removed, err)
	}
}

type stubApps struct{ snapshots []super.Snapshot }

func (s stubApps) Snapshots() []super.Snapshot { return s.snapshots }

func TestRunOnceSkipsAppsThatNeverClean(t *testing.T) {
	off := t.TempDir()
	write(t, filepath.Join(off, "tmp", "old.cache"), 30*24*time.Hour)
	on := t.TempDir()
	write(t, filepath.Join(on, "tmp", "old.cache"), 30*24*time.Hour)

	module := New(stubApps{snapshots: []super.Snapshot{
		{Name: "off", Dir: off},
		{Name: "on", Dir: on, TmpClean: 7 * 24 * time.Hour},
	}})
	module.runOnce(time.Now())

	if _, err := os.Stat(filepath.Join(off, "tmp", "old.cache")); err != nil {
		t.Fatalf("tmp_clean off should keep the file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(on, "tmp", "old.cache")); !os.IsNotExist(err) {
		t.Fatal("tmp_clean on should remove the file")
	}
}
