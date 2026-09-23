package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"dboss/internal/config"
	"dboss/internal/git"
	"dboss/internal/preview"
)

// runHostHook runs a host-level hook. Only the github_pr built-in exists: it deploys or tears
// down one preview app from the request params.
func (s *Service) runHostHook(name string, params map[string]string) error {
	if name != config.BuiltinGithubPR {
		return fmt.Errorf("unknown host hook %q", name)
	}
	cfg := s.runtime.HostConfig()
	hook, ok := cfg.HostHooks[name]
	if !ok {
		return fmt.Errorf("unknown host hook %q", name)
	}
	request, err := preview.Parse(hook, params)
	if err != nil {
		return err
	}
	lock := s.previewLock(request.App)
	lock.Lock()
	defer lock.Unlock()

	actor := "hook:" + name
	detail := fmt.Sprintf("branch=%s num=%s", request.Branch, request.Num)
	if request.Close {
		return s.teardownPreview(cfg, request.App, actor, detail)
	}
	return s.deployPreview(cfg, hook, request, actor, detail)
}

func (s *Service) deployPreview(cfg config.Config, hook config.Hook, request preview.Request, actor, detail string) error {
	appName := request.App
	appDir := filepath.Join(cfg.Apps, appName)
	// Stop a running preview before resetting its checkout, so git never touches files an Odoo
	// process is reading. An unknown app (first deploy) is fine to ignore.
	_ = s.runtime.Stop(appName)
	if err := git.Checkout(appDir, request.Repo, request.Branch, cfg.Defaults.GithubToken); err != nil {
		s.Audit(actor, appName, "deploy", detail, err)
		return err
	}

	rendered, err := preview.Render(hook.Template, request.Vars)
	if err != nil {
		s.Audit(actor, appName, "config-write", detail, err)
		return err
	}
	// A checkout that keeps its app file under config/ gets the rendered one there too.
	configDir := appDir
	if found, err := config.FindInDir(appDir); err == nil {
		configDir = filepath.Dir(found)
	}
	configPath := filepath.Join(configDir, config.FileName)
	// Validate before writing, so a bad template is a clear error rather than a broken app on rescan.
	if _, err := config.ParseApp(rendered, configPath, cfg.Defaults); err != nil {
		s.Audit(actor, appName, "config-write", detail, err)
		return err
	}
	if err := os.WriteFile(configPath, rendered, 0o644); err != nil {
		s.Audit(actor, appName, "config-write", detail, err)
		return err
	}
	s.Audit(actor, appName, "config-write", detail, nil)

	if _, err := s.runtime.Rescan(); err != nil {
		s.Audit(actor, appName, "deploy", detail, err)
		return err
	}
	if _, err := s.runtime.Snapshot(appName); err != nil {
		s.Audit(actor, appName, "deploy", detail, err)
		return fmt.Errorf("app %s did not load after rescan: %w", appName, err)
	}

	if err := s.runtime.Start(appName); err != nil {
		s.Audit(actor, appName, "deploy", detail, err)
		return err
	}
	s.Audit(actor, appName, "deploy", detail, nil)
	return nil
}

func (s *Service) teardownPreview(cfg config.Config, appName, actor, detail string) error {
	appDir := filepath.Join(cfg.Apps, appName)
	if _, err := os.Stat(appDir); err != nil {
		return nil
	}
	_ = s.runtime.Stop(appName)
	s.Audit(actor, appName, "stop", detail, nil)
	if err := s.runtime.Destroy(appName); err != nil {
		s.Audit(actor, appName, "destroy", detail, err)
		return err
	}
	s.Audit(actor, appName, "destroy", detail, nil)
	return nil
}

// previewLock returns the mutex that serializes deploys for one preview app.
func (s *Service) previewLock(name string) *sync.Mutex {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	if s.previewLocks == nil {
		s.previewLocks = map[string]*sync.Mutex{}
	}
	lock := s.previewLocks[name]
	if lock == nil {
		lock = &sync.Mutex{}
		s.previewLocks[name] = lock
	}
	return lock
}
