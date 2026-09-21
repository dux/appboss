package diskusage

import (
	"os"
	"path/filepath"
	"testing"

	"dboss/internal/super"
)

// write creates path with size bytes, making the parent directories as it goes.
func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDirBytesSumsRegularFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "app.rb"), 100)
	write(t, filepath.Join(dir, "public", "assets", "app.js"), 400)

	total, err := DirBytes(dir)
	if err != nil || total != 500 {
		t.Fatalf("DirBytes = %d, %v; want 500", total, err)
	}
}

func TestDirBytesFollowsASymlinkedRoot(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "releases", "2026-09-21")
	write(t, filepath.Join(release, "app.rb"), 250)
	current := filepath.Join(dir, "current")
	if err := os.Symlink(release, current); err != nil {
		t.Fatal(err)
	}

	total, err := DirBytes(current)
	if err != nil || total != 250 {
		t.Fatalf("DirBytes through the release symlink = %d, %v; want 250", total, err)
	}
}

func TestDirBytesDoesNotFollowEntrySymlinks(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared")
	write(t, filepath.Join(shared, "uploads.bin"), 4096)
	app := filepath.Join(dir, "app")
	write(t, filepath.Join(app, "app.rb"), 100)
	if err := os.Symlink(shared, filepath.Join(app, "uploads")); err != nil {
		t.Fatal(err)
	}

	total, err := DirBytes(app)
	if err != nil || total != 100 {
		t.Fatalf("DirBytes = %d, %v; want 100, a shared directory is not billed to the app", total, err)
	}
}

func TestDirBytesMissingDirectoryIsNoError(t *testing.T) {
	total, err := DirBytes(filepath.Join(t.TempDir(), "gone"))
	if err != nil || total != 0 {
		t.Fatalf("DirBytes of a missing directory = %d, %v", total, err)
	}
}

func TestDirBytesKeepsThePartialSumOnAnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "app.rb"), 300)
	locked := filepath.Join(dir, "locked")
	write(t, filepath.Join(locked, "secret.bin"), 700)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	total, err := DirBytes(dir)
	if err == nil {
		t.Fatal("an unreadable directory should be reported")
	}
	if total != 300 {
		t.Fatalf("partial sum = %d, want 300", total)
	}
}

type stubApps struct{ snapshots []super.Snapshot }

func (s stubApps) Snapshots() []super.Snapshot { return s.snapshots }

func TestMeasureCountsLogsOnce(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "apps", "sinatra")
	write(t, filepath.Join(app, "app.rb"), 100)
	logDir := filepath.Join(dir, "log")
	write(t, filepath.Join(logDir, "sinatra", "dboss.sqlite"), 900)

	module := New(stubApps{snapshots: []super.Snapshot{{Name: "sinatra", Dir: app}}}, logDir)
	usage, err := module.Refresh("sinatra")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if usage.AppBytes != 100 || usage.LogBytes != 900 || usage.TotalBytes != 1000 {
		t.Fatalf("usage = %+v, want app 100, logs 900, total 1000", usage)
	}
	if usage.MeasuredAt.IsZero() {
		t.Fatal("a measured app needs a timestamp")
	}
}

// A single-app session resolves log_dir inside the app folder, so the log store must not land in
// both halves of the split.
func TestMeasureKeepsANestedLogDirOutOfAppBytes(t *testing.T) {
	app := t.TempDir()
	write(t, filepath.Join(app, "app.rb"), 100)
	logDir := filepath.Join(app, ".dboss", "log")
	write(t, filepath.Join(logDir, "sinatra", "dboss.sqlite"), 900)

	module := New(stubApps{snapshots: []super.Snapshot{{Name: "sinatra", Dir: app}}}, logDir)
	usage, err := module.Refresh("sinatra")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if usage.AppBytes != 100 || usage.LogBytes != 900 || usage.TotalBytes != 1000 {
		t.Fatalf("usage = %+v, want app 100, logs 900, total 1000", usage)
	}
}

func TestUsageIsUnsetBeforeTheFirstMeasure(t *testing.T) {
	module := New(stubApps{}, t.TempDir())
	if usage, ok := module.Usage("sinatra"); ok {
		t.Fatalf("Usage before the first pass = %+v, want unmeasured", usage)
	}
}

func TestRefreshRejectsAnUnknownApp(t *testing.T) {
	module := New(stubApps{}, t.TempDir())
	if _, err := module.Refresh("nope"); err == nil {
		t.Fatal("an unknown app should be an error")
	}
}

func TestRunOnceDropsAppsThatDisappeared(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	write(t, filepath.Join(first, "app.rb"), 10)
	write(t, filepath.Join(second, "app.rb"), 20)

	apps := &stubApps{snapshots: []super.Snapshot{{Name: "first", Dir: first}, {Name: "second", Dir: second}}}
	module := New(apps, filepath.Join(dir, "log"))
	module.runOnce()
	if _, ok := module.Usage("second"); !ok {
		t.Fatal("second should be measured")
	}

	apps.snapshots = apps.snapshots[:1]
	module.runOnce()
	if usage, ok := module.Usage("second"); ok {
		t.Fatalf("a removed app should be forgotten, got %+v", usage)
	}
	if _, ok := module.Usage("first"); !ok {
		t.Fatal("first should still be measured")
	}
}

func TestMeasureSkipsAWalkThatIsAlreadyRunning(t *testing.T) {
	app := t.TempDir()
	write(t, filepath.Join(app, "app.rb"), 100)
	snapshot := super.Snapshot{Name: "sinatra", Dir: app}
	module := New(stubApps{snapshots: []super.Snapshot{snapshot}}, filepath.Join(app, "log"))
	if _, err := module.measure(snapshot); err != nil {
		t.Fatalf("measure: %v", err)
	}

	// Pretend a walk is in flight: the second caller answers from the cache instead of queueing
	// another walk of the same tree.
	write(t, filepath.Join(app, "grown.bin"), 500)
	module.claim("sinatra")
	usage, err := module.measure(snapshot)
	if err != nil {
		t.Fatalf("measure while busy: %v", err)
	}
	if usage.AppBytes != 100 {
		t.Fatalf("app bytes = %d, want the cached 100", usage.AppBytes)
	}
	module.release("sinatra")
}
