package supervisor

import (
	"fmt"
	"net/url"
)

// RunHook starts one deploy hook now, whether the app is running or stopped.
func (m *Manager) RunHook(name, hookName string) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	return runtime.call(request{kind: requestHookRun, hook: hookName})
}

// Hooks returns every hook of one app with its ping URL. It is the only read path that carries
// the token, so it stays off the regular snapshot.
func (m *Manager) Hooks(name string) ([]HookInfo, error) {
	runtime, err := m.runtime(name)
	if err != nil {
		return nil, err
	}
	response := runtime.query(request{kind: requestHooks})
	if response.err != nil {
		return nil, response.err
	}
	host := m.HostConfig()
	result := make([]HookInfo, 0, len(response.hooks))
	for _, snapshot := range response.hooks {
		result = append(result, HookInfo{HookSnapshot: snapshot, URL: hookLink(host.ConsoleURL(), host.Tokens.Dboss, name, snapshot.Name)})
	}
	return result, nil
}

// HookToken returns the token a ping to one app hook must present: tokens.dboss of the live host
// config. An unknown app or hook is an error, so the endpoint can answer 404.
func (m *Manager) HookToken(name, hookName string) (string, error) {
	hooks, err := m.Hooks(name)
	if err != nil {
		return "", err
	}
	for _, info := range hooks {
		if info.Name == hookName {
			return m.HostConfig().Tokens.Dboss, nil
		}
	}
	return "", fmt.Errorf("unknown hook %q", hookName)
}

// HostHookToken is HookToken for a host-level hook.
func (m *Manager) HostHookToken(name string) (string, error) {
	host := m.HostConfig()
	if _, ok := host.HostHooks[name]; !ok {
		return "", fmt.Errorf("unknown host hook %q", name)
	}
	return host.Tokens.Dboss, nil
}

// HookInfo is one hook with its ready-made ping URL, returned by the dedicated hooks view and
// the CLI. It never ships inside a regular app snapshot.
type HookInfo struct {
	HookSnapshot
	URL string `json:"url"`
}

// hookLink is the ping URL of one hook with the token in the query, or empty when the console
// has no address or no token is set.
func hookLink(base, token, app, name string) string {
	if base == "" || token == "" {
		return ""
	}
	return base + "/hooks/" + url.PathEscape(app) + "/" + url.PathEscape(name) + "?token=" + url.QueryEscape(token)
}
