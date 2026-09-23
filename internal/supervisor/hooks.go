package supervisor

import (
	"errors"
	"fmt"
	"net/url"

	"dboss/internal/apps"
)

// RunHook starts one deploy hook now, whether the app is running or stopped.
func (m *Manager) RunHook(name, hookName string) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	return runtime.call(request{kind: requestHookRun, hook: hookName})
}

// Hooks returns every hook of one app with its effective secret. It is the only read path that
// carries secrets, so it stays off the regular snapshot.
func (m *Manager) Hooks(name string) ([]HookInfo, error) {
	runtime, err := m.runtime(name)
	if err != nil {
		return nil, err
	}
	response := runtime.query(request{kind: requestHooks})
	return response.hooks, response.err
}

// HookSecret returns the effective secret of one hook, for verifying a ping.
func (m *Manager) HookSecret(name, hookName string) (string, error) {
	hooks, err := m.Hooks(name)
	if err != nil {
		return "", err
	}
	for _, info := range hooks {
		if info.Name == hookName {
			if info.Secret == "" {
				return "", fmt.Errorf("hook %q has no secret", hookName)
			}
			return info.Secret, nil
		}
	}
	return "", fmt.Errorf("unknown hook %q", hookName)
}

// HostHookSecret returns the effective secret of a host-level hook: the config value when set,
// else the generated one. Host hooks share the generated store under the reserved app name
// "_host", so a host hook rotates and persists like an app hook.
func (m *Manager) HostHookSecret(name string) (string, error) {
	hook, ok := m.HostConfig().HostHooks[name]
	if !ok {
		return "", fmt.Errorf("unknown host hook %q", name)
	}
	if hook.Secret != "" {
		return hook.Secret, nil
	}
	return m.secrets.Ensure("_host", name)
}

// RotateHook mints a new generated secret for one hook and returns the hook with its new URL. A
// hook whose secret comes from the config is rejected: the operator changes it in the file.
func (m *Manager) RotateHook(name, hookName string) (HookInfo, error) {
	runtime, err := m.runtime(name)
	if err != nil {
		return HookInfo{}, err
	}
	response := runtime.query(request{kind: requestHookRotate, hook: hookName})
	if response.err != nil {
		return HookInfo{}, response.err
	}
	hooks, err := m.Hooks(name)
	if err != nil {
		return HookInfo{}, err
	}
	for _, info := range hooks {
		if info.Name == hookName {
			return info, nil
		}
	}
	return HookInfo{}, fmt.Errorf("unknown hook %q", hookName)
}

// syncHookSecrets mints and prunes generated hook secrets against the discovered specs, using
// the same list the runtimes were built from so no running spec is read off its goroutine.
func (m *Manager) syncHookSecrets(discovered []*apps.App) {
	if m.secrets == nil {
		return
	}
	live := map[string]map[string]bool{}
	for _, spec := range discovered {
		hooks := map[string]bool{}
		for name, hookConfig := range spec.Hooks {
			if hookConfig.Disabled {
				continue
			}
			hooks[name] = true
			if configured, ok := spec.Config.Hooks[name]; ok && configured.Secret != "" {
				continue
			}
			_, _ = m.secrets.Ensure(spec.Name, name)
		}
		live[spec.Name] = hooks
	}
	_ = m.secrets.Reconcile(live)
}

// rotateHook mints a new generated secret for one hook. A hook whose secret lives in the config
// cannot be rotated here.
func (a *appRuntime) rotateHook(name string) error {
	if a.hooks[name] == nil {
		return fmt.Errorf("unknown hook %q", name)
	}
	if configured, ok := a.spec.Config.Hooks[name]; ok && configured.Secret != "" {
		return fmt.Errorf("hook %q uses a secret from the config; change it there", name)
	}
	if a.secrets == nil {
		return errors.New("hook secret store is not available")
	}
	_, err := a.secrets.Rotate(a.spec.Name, name)
	return err
}

// HookInfo is one hook with its effective secret and ready-made ping URL, returned by the
// dedicated hooks view and the CLI. It never ships inside a regular app snapshot.
type HookInfo struct {
	HookSnapshot
	Secret string `json:"secret"`
	Source string `json:"source"` // config or generated
	URL    string `json:"url"`
}

// hookInfos pairs each hook snapshot with its effective secret and ping URL. A secret set in
// the config wins; otherwise the generated one from state_dir is used and the source is reported
// so the operator knows where to rotate it.
func (a *appRuntime) hookInfos() []HookInfo {
	snapshots := a.hookSnapshot()
	base := a.cfg.ConsoleURL()
	result := make([]HookInfo, 0, len(snapshots))
	for _, snapshot := range snapshots {
		secret, source := "", ""
		if configured, ok := a.spec.Config.Hooks[snapshot.Name]; ok && configured.Secret != "" {
			secret, source = configured.Secret, "config"
		} else if a.secrets != nil {
			secret, source = a.secrets.Get(a.spec.Name, snapshot.Name), "generated"
		}
		result = append(result, HookInfo{HookSnapshot: snapshot, Secret: secret, Source: source, URL: hookLink(base, a.spec.Name, snapshot.Name, secret)})
	}
	return result
}

func hookLink(base, app, name, secret string) string {
	if base == "" {
		return ""
	}
	link := base + "/hooks/" + url.PathEscape(app) + "/" + url.PathEscape(name)
	if secret != "" {
		link += "?token=" + url.QueryEscape(secret)
	}
	return link
}
