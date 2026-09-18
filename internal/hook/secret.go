// Package hook owns the generated secret for a deploy hook. A secret set in the app config
// wins; the one here is the fallback that makes a ping URL work without any config and stays on
// the server, like management-auth.key.
package hook

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const fileName = "hook-secrets.json"

// Store is the state_dir/hook-secrets.json map of app -> hook -> secret.
type Store struct {
	mu   sync.Mutex
	path string
	data map[string]map[string]string
}

func Open(stateDir string) (*Store, error) {
	store := &Store{path: filepath.Join(stateDir, fileName), data: map[string]map[string]string{}}
	data, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &store.data); err != nil {
		return nil, err
	}
	return store, nil
}

// Get returns the generated secret, or "" when none exists.
func (s *Store) Get(app, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[app][name]
}

// Ensure returns the generated secret for a hook, creating and persisting one on first use.
func (s *Store) Ensure(app, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if secret := s.data[app][name]; secret != "" {
		return secret, nil
	}
	secret, err := Generate()
	if err != nil {
		return "", err
	}
	s.set(app, name, secret)
	return secret, s.save()
}

// Set replaces the generated secret for a hook.
func (s *Store) Set(app, name, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set(app, name, secret)
	return s.save()
}

// Reconcile drops generated secrets for apps and hooks that no longer exist, so a removed hook
// does not keep a valid token around.
func (s *Store) Reconcile(live map[string]map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for app, hooks := range s.data {
		keep, ok := live[app]
		if !ok {
			delete(s.data, app)
			changed = true
			continue
		}
		for name := range hooks {
			if !keep[name] {
				delete(hooks, name)
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return s.save()
}

func (s *Store) set(app, name, secret string) {
	if s.data[app] == nil {
		s.data[app] = map[string]string{}
	}
	s.data[app][name] = secret
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	newPath := s.path + ".new"
	if err := os.WriteFile(newPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(newPath, s.path); err != nil {
		return err
	}
	return os.Chmod(s.path, 0o600)
}

// Generate returns a 64-character URL-safe secret from 48 random bytes.
func Generate() (string, error) {
	var raw [48]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
