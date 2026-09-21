// Package diskusage measures what each app costs on disk: its own directory plus the log store
// dboss writes for it. A walk of a release tree is far too slow for a page load, so the module
// keeps the last result per app, refreshes it once a day, and re-measures one app on demand.
package diskusage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	"dboss/internal/logx"
	"dboss/internal/super"
)

// interval is how often every app is measured.
const interval = 24 * time.Hour

// Snapshotter lists the apps to measure and where they live.
type Snapshotter interface {
	Snapshots() []super.Snapshot
}

// Usage is one app's footprint, split into the app's own files and the log store dboss keeps for
// it. A zero MeasuredAt means the app has not been walked yet, which is not the same as empty.
type Usage struct {
	AppBytes   int64
	LogBytes   int64
	TotalBytes int64
	MeasuredAt time.Time
}

// Module measures every app on a timer and answers from its cache.
type Module struct {
	apps   Snapshotter
	logDir string
	cancel context.CancelFunc
	done   chan struct{}

	// walk serializes the walks: two at once on one disk are slower than one after the other.
	// mu only ever guards the maps, never a walk.
	walk      sync.Mutex
	mu        sync.RWMutex
	usage     map[string]Usage
	measuring map[string]bool
}

func New(apps Snapshotter, logDir string) *Module {
	return &Module{apps: apps, logDir: logDir, done: make(chan struct{}), usage: map[string]Usage{}, measuring: map[string]bool{}}
}

func (m *Module) Name() string { return "diskusage" }

func (m *Module) Start(ctx context.Context) error {
	ctx, m.cancel = context.WithCancel(ctx)
	go m.loop(ctx)
	return nil
}

func (m *Module) Close() error {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
	return nil
}

// loop measures once on start, so the first console load after a restart already has numbers.
func (m *Module) loop(ctx context.Context) {
	defer close(m.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		m.runOnce()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Usage returns the last measurement of one app. The second result is false until it has been
// measured, so a caller can tell an unmeasured app from an empty one.
func (m *Module) Usage(app string) (Usage, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	usage, ok := m.usage[app]
	return usage, ok
}

// Refresh measures one app now instead of waiting for the daily pass. An unknown app is an error.
func (m *Module) Refresh(app string) (Usage, error) {
	for _, snapshot := range m.apps.Snapshots() {
		if snapshot.Name == app {
			return m.measure(snapshot)
		}
	}
	return Usage{}, fmt.Errorf("unknown app %q", app)
}

// runOnce measures every app and forgets the ones that are gone, so a destroyed app stops being
// reported.
func (m *Module) runOnce() {
	snapshots := m.apps.Snapshots()
	live := make(map[string]bool, len(snapshots))
	for _, snapshot := range snapshots {
		live[snapshot.Name] = true
		if _, err := m.measure(snapshot); err != nil {
			logx.Warnf("disk usage %s: %v", snapshot.Name, err)
		}
	}
	m.mu.Lock()
	for name := range m.usage {
		if !live[name] {
			delete(m.usage, name)
		}
	}
	m.mu.Unlock()
}

// measure walks one app and caches what it found. A walk that could not read everything still
// stores the partial sum, so the console shows a number rather than nothing. While a walk for this
// app is already running the cached value is returned instead of queueing a second one.
func (m *Module) measure(snapshot super.Snapshot) (Usage, error) {
	if !m.claim(snapshot.Name) {
		usage, _ := m.Usage(snapshot.Name)
		return usage, nil
	}
	defer m.release(snapshot.Name)

	m.walk.Lock()
	defer m.walk.Unlock()
	logPath := filepath.Join(m.logDir, snapshot.Name)
	// In a single-app session log_dir resolves inside the app folder, so the log store would be
	// counted twice. Skipping it in the app walk keeps the split honest in either layout.
	logRoot, err := filepath.EvalSymlinks(logPath)
	if err != nil {
		logRoot = ""
	}
	appBytes, appErr := dirBytes(snapshot.Dir, logRoot)
	logBytes, logErr := DirBytes(logPath)
	usage := Usage{AppBytes: appBytes, LogBytes: logBytes, TotalBytes: appBytes + logBytes, MeasuredAt: time.Now()}
	m.mu.Lock()
	m.usage[snapshot.Name] = usage
	m.mu.Unlock()
	return usage, errors.Join(appErr, logErr)
}

// claim reports whether this caller is the one that measures the app now.
func (m *Module) claim(app string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.measuring[app] {
		return false
	}
	m.measuring[app] = true
	return true
}

func (m *Module) release(app string) {
	m.mu.Lock()
	delete(m.measuring, app)
	m.mu.Unlock()
}

// DirBytes sums the size of every regular file under dir. It is apparent size, what `du
// --apparent-size` prints: a sparse file counts as written and two hard links to one file count
// twice. A missing directory is 0, and a file that cannot be read is skipped and reported, so the
// caller still gets the rest of the sum.
func DirBytes(dir string) (int64, error) { return dirBytes(dir, "") }

// dirBytes is DirBytes with one subtree left out. skip is an already resolved path; an empty one
// never matches.
func dirBytes(dir, skip string) (int64, error) {
	// An app entry is usually a symlink to the current release, so the root is resolved before the
	// walk; entries below it are not, so a tmp or log symlink out of the app counts as the link
	// itself and a shared directory is never billed to two apps.
	root, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	var failure error
	fail := func(err error) {
		if failure == nil {
			failure = err
		}
	}
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			fail(err)
			return nil
		}
		if entry.IsDir() {
			if skip != "" && path == skip {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			fail(err)
			return nil
		}
		total += info.Size()
		return nil
	})
	if walkErr != nil {
		fail(walkErr)
	}
	return total, failure
}
