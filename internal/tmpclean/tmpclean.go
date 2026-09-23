// Package tmpclean keeps each app's ./tmp directory from growing forever. Apps drop caches,
// sockets, uploads and pids there and never come back for them, so once a day dboss removes
// everything older than the app's tmp_clean age. It keeps no state: the age rides the snapshot.
package tmpclean

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"dboss/internal/logx"
	"dboss/internal/module"
	"dboss/internal/supervisor"
)

// interval is how often every app is swept. The age itself is per app.
const interval = 24 * time.Hour

// Snapshotter lists the apps to sweep.
type Snapshotter interface {
	Snapshots() []supervisor.Snapshot
}

// Module sweeps the tmp directory of every app on a timer.
type Module struct {
	apps Snapshotter
	loop module.Ticker
}

func New(apps Snapshotter) *Module { return &Module{apps: apps} }

func (m *Module) Name() string { return "tmpclean" }

func (m *Module) Start(ctx context.Context) error {
	// Sweeps once on start, so a box that is restarted daily is still cleaned.
	m.loop.Run(ctx, interval, true, func(context.Context) { m.runOnce(time.Now()) })
	return nil
}

func (m *Module) Close() error { return m.loop.Close() }

func (m *Module) runOnce(now time.Time) {
	for _, snapshot := range m.apps.Snapshots() {
		if snapshot.TmpClean <= 0 {
			continue
		}
		removed, err := Sweep(filepath.Join(snapshot.Dir, "tmp"), now.Add(-snapshot.TmpClean))
		if err != nil {
			logx.Warnf("tmp clean %s: %v", snapshot.Name, err)
		}
		if removed > 0 {
			logx.Infof("tmp clean %s: removed %d files", snapshot.Name, removed)
		}
	}
}

// Sweep deletes every file under dir last modified before cutoff, then the sub-directories the
// deletion left empty and the ones that were already empty and as old. dir itself always stays,
// and a missing one is not an error. It returns the number of files removed and the first error
// that stopped one from going.
func Sweep(dir string, cutoff time.Time) (int, error) {
	// The app directory is often a release symlink, and so is tmp itself on a shared-directory
	// deploy; WalkDir does not follow either, so resolve the root before walking it.
	root, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, nil
	}
	removed := 0
	var dirs []string
	// Directories this sweep deleted from: removing a child moves the parent's mtime to now, so
	// its own age can no longer say whether it is stale.
	emptied := map[string]bool{}
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
		if path == root {
			return nil
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			fail(err)
			return nil
		}
		if !info.ModTime().Before(cutoff) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			fail(err)
			return nil
		}
		removed++
		emptied[filepath.Dir(path)] = true
		return nil
	})
	if walkErr != nil {
		fail(walkErr)
	}
	// Deepest first, so a directory whose children just went is empty by the time it is tried.
	// os.Remove refuses a directory that still holds anything, which is the check we want.
	for i := len(dirs) - 1; i >= 0; i-- {
		path := dirs[i]
		if !emptied[path] {
			info, err := os.Lstat(path)
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
		}
		if os.Remove(path) == nil {
			emptied[filepath.Dir(path)] = true
		}
	}
	return removed, failure
}
