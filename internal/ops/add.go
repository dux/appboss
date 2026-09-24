package ops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/git"
	"dboss/internal/supervisor"
)

// normalizeRepo is the test seam: tests clone from a local path, which the real one refuses.
var normalizeRepo = git.NormalizeRepo

// addName is the app name an add request creates: the one given, else the repository's.
func addName(request Request) string {
	if request.App != "" {
		return request.App
	}
	if _, name, err := normalizeRepo(request.Repo); err == nil {
		return name
	}
	return ""
}

// add clones a repository that carries its own dboss.yaml into the apps folder, loads it and
// starts it. Host, when set, replaces the single web process's hosts through a dboss.local.yaml.
// Anything that fails after the clone removes the folder again, so a retry starts clean.
func (s *Service) add(request Request) (supervisor.Snapshot, error) {
	cfg := s.runtime.HostConfig()
	if cfg.Dev() {
		return supervisor.Snapshot{}, errors.New("a single-app session has no apps folder to add to")
	}
	repo, _, err := normalizeRepo(request.Repo)
	if err != nil {
		return supervisor.Snapshot{}, err
	}
	name := addName(request)
	if err := config.ValidAppName(name); err != nil {
		return supervisor.Snapshot{}, err
	}
	lock := s.previewLock(name)
	lock.Lock()
	defer lock.Unlock()

	dir := filepath.Join(cfg.Apps, name)
	if _, err := os.Lstat(dir); err == nil {
		return supervisor.Snapshot{}, fmt.Errorf("app %s already exists; deploy to it instead", name)
	}
	if err := git.Clone(dir, repo, request.Branch, cfg.Tokens.Github); err != nil {
		_ = os.RemoveAll(dir)
		return supervisor.Snapshot{}, err
	}
	snapshot, loaded, err := s.loadAdded(cfg, name, dir, request.Host)
	if err != nil {
		_ = apps.Destroy(cfg.Apps, name)
		if loaded {
			_, _ = s.runtime.Rescan()
		}
		return supervisor.Snapshot{}, err
	}
	return snapshot, nil
}

// loadAdded validates the fresh checkout, applies the host override, rescans and starts it.
// loaded reports whether the rescan picked the app up, so a failure can rescan it away again.
func (s *Service) loadAdded(cfg config.Config, name, dir, host string) (supervisor.Snapshot, bool, error) {
	path, err := config.FindInDir(dir)
	if errors.Is(err, config.ErrNoConfig) {
		return supervisor.Snapshot{}, false, fmt.Errorf("the repository has no %s, so it is not a dboss app", config.FileName)
	}
	if err != nil {
		return supervisor.Snapshot{}, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return supervisor.Snapshot{}, false, err
	}
	app, err := config.ParseApp(data, path, cfg.Defaults)
	if err != nil {
		return supervisor.Snapshot{}, false, err
	}
	if host != "" {
		if app, err = overrideHost(app, data, path, host, cfg.Defaults); err != nil {
			return supervisor.Snapshot{}, false, err
		}
	}
	// Rescan hands a contested host to whichever app sorts first, which could take it from a
	// running app and stop that app, so a clash is refused before the rescan.
	for _, snapshot := range s.runtime.Snapshots() {
		for _, owned := range snapshot.Hosts {
			for _, wanted := range app.Hosts {
				if config.NormalizePattern(owned) == config.NormalizePattern(wanted) {
					return supervisor.Snapshot{}, false, fmt.Errorf("host %s is already served by app %s", wanted, snapshot.Name)
				}
			}
		}
	}

	invalid, err := s.runtime.Rescan()
	if err != nil {
		return supervisor.Snapshot{}, false, err
	}
	snapshot, err := s.runtime.Snapshot(name)
	if err != nil {
		for _, scanErr := range invalid {
			var scan apps.ScanError
			if errors.As(scanErr, &scan) && scan.Name == name {
				return supervisor.Snapshot{}, false, scan.Err
			}
		}
		return supervisor.Snapshot{}, false, fmt.Errorf("app %s did not load after rescan: %w", name, err)
	}
	if s.pubsub != nil {
		s.pubsub.Reconcile(s.runtime.Snapshots())
	}
	if err := s.runtime.Start(name); err != nil {
		return supervisor.Snapshot{}, true, err
	}
	if fresh, err := s.runtime.Snapshot(name); err == nil {
		snapshot = fresh
	}
	return snapshot, true, nil
}

// overrideHost writes the checkout's config to dboss.local.yaml with host as the single web
// process's only host. The local file replaces the committed one, so it is a full copy, and the
// canonical host goes because it must be one of the hosts.
func overrideHost(app config.App, data []byte, path, host string, defaults config.Defaults) (config.App, error) {
	if len(app.WebProcesses) != 1 {
		return config.App{}, fmt.Errorf("a host override needs exactly one web process, the app has %d", len(app.WebProcesses))
	}
	web := app.WebProcesses[0].Name
	patched, err := config.PatchYAML(string(data), map[string]any{"procfile." + web + ".hosts": []string{config.NormalizeHost(host)}}, []string{"procfile." + web + ".canonical_host"})
	if err != nil {
		return config.App{}, fmt.Errorf("cannot set the host of procfile.%s: %w", web, err)
	}
	local := filepath.Join(filepath.Dir(path), config.LocalFileName)
	overridden, err := config.ParseApp([]byte(patched), local, defaults)
	if err != nil {
		return config.App{}, err
	}
	if err := os.WriteFile(local, []byte(patched), 0o644); err != nil {
		return config.App{}, err
	}
	return overridden, nil
}
