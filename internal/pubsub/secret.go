package pubsub

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const secretFile = "pubsub-secrets.json"

// secretStore is the state_dir/pubsub-secrets.json map of app -> publish secret. It is the
// fallback when the app config sets no secret, so publishing works without any committed value.
type secretStore struct {
	mu   sync.Mutex
	path string
	data map[string]string
}

func openSecrets(stateDir string) (*secretStore, error) {
	store := &secretStore{path: filepath.Join(stateDir, secretFile), data: map[string]string{}}
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

func (s *secretStore) ensure(app string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if secret := s.data[app]; secret != "" {
		return secret, nil
	}
	secret, err := generateSecret()
	if err != nil {
		return "", err
	}
	s.data[app] = secret
	return secret, s.save()
}

func (s *secretStore) set(app, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[app] = secret
	return s.save()
}

// reconcile drops generated secrets for apps that no longer exist.
func (s *secretStore) reconcile(live map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for app := range s.data {
		if !live[app] {
			delete(s.data, app)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save()
}

func (s *secretStore) save() error {
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

func generateSecret() (string, error) {
	var raw [48]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
