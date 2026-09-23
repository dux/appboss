// Package secret owns generated secrets: the fallback that makes a deploy hook ping URL or a
// pubsub publish work without any committed value. A secret set in the config always wins; the one
// here stays on the server, like management-auth.key.
package secret

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"

	"dboss/internal/fsutil"
)

// Store is one state_dir JSON file mapping group -> name -> secret, e.g. app -> hook.
type Store struct {
	mu   sync.Mutex
	path string
	data map[string]map[string]string
}

// Open reads the store at path. A missing file is an empty store; an unreadable one is an error,
// because saving over it would silently rotate every secret it held.
func Open(path string) (*Store, error) {
	store := &Store{path: path, data: map[string]map[string]string{}}
	if err := fsutil.ReadJSON(path, &store.data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return store, nil
}

// Get returns the generated secret, or "" when none exists.
func (s *Store) Get(group, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[group][name]
}

// Ensure returns the generated secret, creating and persisting one on first use.
func (s *Store) Ensure(group, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if secret := s.data[group][name]; secret != "" {
		return secret, nil
	}
	return s.replace(group, name)
}

// Rotate replaces the generated secret with a fresh one and returns it.
func (s *Store) Rotate(group, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replace(group, name)
}

// Reconcile drops generated secrets whose group or name is no longer live, so a removed hook or
// hub does not keep a valid token around.
func (s *Store) Reconcile(live map[string]map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for group, names := range s.data {
		keep, ok := live[group]
		if !ok {
			delete(s.data, group)
			changed = true
			continue
		}
		for name := range names {
			if !keep[name] {
				delete(names, name)
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return s.save()
}

func (s *Store) replace(group, name string) (string, error) {
	secret, err := Generate()
	if err != nil {
		return "", err
	}
	if s.data[group] == nil {
		s.data[group] = map[string]string{}
	}
	s.data[group][name] = secret
	return secret, s.save()
}

func (s *Store) save() error {
	return fsutil.WriteJSON(s.path, s.data, 0o600)
}

// Generate returns a 64-character URL-safe secret from 48 random bytes.
func Generate() (string, error) {
	var raw [48]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
