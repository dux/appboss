package apps

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"app-boss/internal/config"
	"gopkg.in/yaml.v3"
)

// ErrConflict is returned by Write when the file on disk no longer matches the revision the
// caller edited. The returned ConfigFile then carries the current contents.
var ErrConflict = errors.New("file changed on disk")

// historyKeep is how many past revisions of one file are kept under state_dir/config-history.
const historyKeep = 50

// ConfigRevision is one saved revision of a config file.
type ConfigRevision struct {
	ID       string    `json:"id"`
	App      string    `json:"app,omitempty"`
	Revision string    `json:"revision"`
	Time     time.Time `json:"time"`
	Source   string    `json:"source"`
	name     string
}

// ConfigFile is one file appboss reads. IDs are "host" or "app:<name>" and map to paths only on
// the server, so a client never names a path.
type ConfigFile struct {
	ID       string `json:"id"`
	App      string `json:"app,omitempty"`
	Path     string `json:"path"`
	Source   string `json:"source"`
	Contents string `json:"contents,omitempty"`
	Revision string `json:"revision"`
	HasLocal bool   `json:"has_local"`
}

// Store edits the real config files. The root config is the one the session started with:
// its apps directory and source path are host keys that only change with a restart.
type Store struct {
	root    config.Config
	history string
}

func NewStore(root config.Config) *Store {
	return &Store{root: root, history: filepath.Join(root.StateDir, "config-history")}
}

// Files lists the host file and, in host mode, the active file of every app folder.
func (s *Store) Files() ([]ConfigFile, error) {
	hostPath, err := config.FindInDir(s.root.Dir)
	if err != nil {
		hostPath = s.root.SourcePath
	}
	_, localErr := os.Stat(filepath.Join(s.root.Dir, config.LocalFileName))
	files := []ConfigFile{{ID: "host", Path: hostPath, Source: filepath.Base(hostPath), HasLocal: localErr == nil}}
	if s.root.App != nil {
		// Single mode: the host file is the app file, and the app is named after its folder.
		files[0].App = filepath.Base(s.root.Dir)
		return s.stat(files)
	}
	names, err := appNames(s.root.Apps)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		dir := filepath.Join(s.root.Apps, name)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		path, err := config.FindInDir(dir)
		if err != nil {
			continue
		}
		_, localErr := os.Stat(filepath.Join(dir, config.LocalFileName))
		files = append(files, ConfigFile{ID: "app:" + name, App: name, Path: path, Source: filepath.Base(path), HasLocal: localErr == nil})
	}
	return s.stat(files)
}

func (s *Store) stat(files []ConfigFile) ([]ConfigFile, error) {
	for i := range files {
		data, err := os.ReadFile(files[i].Path)
		if err != nil {
			return nil, err
		}
		files[i].Revision = revision(data)
	}
	return files, nil
}

func (s *Store) Read(id string) (ConfigFile, error) {
	file, err := s.lookup(id)
	if err != nil {
		return ConfigFile{}, err
	}
	data, err := os.ReadFile(file.Path)
	if err != nil {
		return ConfigFile{}, err
	}
	file.Contents, file.Revision = string(data), revision(data)
	return file, nil
}

func (s *Store) lookup(id string) (ConfigFile, error) {
	files, err := s.Files()
	if err != nil {
		return ConfigFile{}, err
	}
	for _, file := range files {
		if file.ID == id {
			return file, nil
		}
	}
	return ConfigFile{}, fmt.Errorf("unknown config file %q", id)
}

// Validate runs contents through the real loader for the file's role: the host file as a root
// document that keeps its role, an app file as a child under the host defaults on disk.
func (s *Store) Validate(id, contents string) error {
	file, err := s.lookup(id)
	if err != nil {
		return err
	}
	if id == "host" {
		parsed, err := config.Parse([]byte(contents), file.Path)
		if err != nil {
			return err
		}
		if (parsed.App != nil) != (s.root.App != nil) {
			return errors.New("the host file cannot switch between apps and procfile while appboss runs")
		}
		if parsed.App != nil {
			_, err = validateApp(*parsed.App)
		}
		return err
	}
	root, err := s.HostConfig()
	if err != nil {
		return fmt.Errorf("host file: %w", err)
	}
	app, err := config.ParseApp([]byte(contents), file.Path, root.Defaults)
	if err != nil {
		return err
	}
	_, err = validateApp(app)
	return err
}

// Write validates contents, refuses when the file on disk is not at revision, then replaces the
// file atomically keeping its mode.
func (s *Store) Write(id, contents, revision string) (ConfigFile, error) {
	current, err := s.Read(id)
	if err != nil {
		return ConfigFile{}, err
	}
	if current.Revision != revision {
		return current, ErrConflict
	}
	if err := s.Validate(id, contents); err != nil {
		return ConfigFile{}, err
	}
	if err := s.snapshot(id, current); err != nil {
		return ConfigFile{}, err
	}
	if err := replaceFile(current.Path, []byte(contents)); err != nil {
		return ConfigFile{}, err
	}
	return s.Read(id)
}

// History lists the saved revisions of one config file, newest first.
func (s *Store) History(id string) ([]ConfigRevision, error) {
	file, err := s.lookup(id)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.history)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prefix := safeID(id) + "__"
	var result []ConfigRevision
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".yaml"), "__")
		if len(parts) != 2 {
			continue
		}
		nanos, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			continue
		}
		result = append(result, ConfigRevision{ID: id, App: file.App, Revision: parts[1], Time: time.Unix(0, nanos), Source: file.Source, name: name})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Time.After(result[j].Time) })
	return result, nil
}

// HistoryContents returns a saved revision as written.
func (s *Store) HistoryContents(id, revision string) (string, error) {
	revisions, err := s.History(id)
	if err != nil {
		return "", err
	}
	for _, entry := range revisions {
		if entry.Revision == revision {
			data, err := os.ReadFile(filepath.Join(s.history, entry.name))
			return string(data), err
		}
	}
	return "", fmt.Errorf("unknown revision %q for %s", revision, id)
}

// Restore writes a saved revision back as the current file. It is revision-checked against the
// live file, so a concurrent edit is a conflict rather than a silent overwrite.
func (s *Store) Restore(id, revision string) (ConfigFile, error) {
	contents, err := s.HistoryContents(id, revision)
	if err != nil {
		return ConfigFile{}, err
	}
	current, err := s.Read(id)
	if err != nil {
		return ConfigFile{}, err
	}
	return s.Write(id, contents, current.Revision)
}

// snapshot keeps the current file contents before a write replaces them.
func (s *Store) snapshot(id string, file ConfigFile) error {
	if s.history == "" || file.Contents == "" {
		return nil
	}
	if err := os.MkdirAll(s.history, 0o750); err != nil {
		return err
	}
	name := filepath.Join(s.history, fmt.Sprintf("%s__%d__%s.yaml", safeID(id), time.Now().UnixNano(), file.Revision))
	if err := os.WriteFile(name, []byte(file.Contents), 0o640); err != nil {
		return err
	}
	return s.pruneHistory(id)
}

func (s *Store) pruneHistory(id string) error {
	revisions, err := s.History(id)
	if err != nil {
		return err
	}
	for _, old := range revisions[min(len(revisions), historyKeep):] {
		_ = os.Remove(filepath.Join(s.history, old.name))
	}
	return nil
}

func safeID(id string) string { return strings.ReplaceAll(id, ":", "-") }

// CreateLocal copies an app's appboss.yaml to appboss.local.yaml so edits made on the server live
// in the file the next deploy does not overwrite.
func (s *Store) CreateLocal(app string) (ConfigFile, error) {
	file, err := s.lookup("app:" + app)
	if err != nil {
		return ConfigFile{}, err
	}
	if file.HasLocal {
		return ConfigFile{}, fmt.Errorf("%s already has %s", app, config.LocalFileName)
	}
	info, err := os.Stat(file.Path)
	if err != nil {
		return ConfigFile{}, err
	}
	data, err := os.ReadFile(file.Path)
	if err != nil {
		return ConfigFile{}, err
	}
	target := filepath.Join(filepath.Dir(file.Path), config.LocalFileName)
	handle, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return ConfigFile{}, err
	}
	if _, err := handle.Write(data); err != nil {
		_ = handle.Close()
		return ConfigFile{}, err
	}
	if err := handle.Close(); err != nil {
		return ConfigFile{}, err
	}
	return s.Read("app:" + app)
}

// CreateHostLocal copies the host config to appboss.local.yaml so console writes there survive a
// deploy. It is a no-op when the local file already exists.
func (s *Store) CreateHostLocal() (ConfigFile, error) {
	target := filepath.Join(s.root.Dir, config.LocalFileName)
	if _, err := os.Stat(target); err == nil {
		return s.Read("host")
	}
	info, err := os.Stat(s.root.SourcePath)
	if err != nil {
		return ConfigFile{}, err
	}
	data, err := os.ReadFile(s.root.SourcePath)
	if err != nil {
		return ConfigFile{}, err
	}
	handle, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return ConfigFile{}, err
	}
	if _, err := handle.Write(data); err != nil {
		_ = handle.Close()
		return ConfigFile{}, err
	}
	if err := handle.Close(); err != nil {
		return ConfigFile{}, err
	}
	return s.Read("host")
}

// HostConfig loads the active host config from disk, so a caller can push a fresh value into a
// service after a console write.
func (s *Store) HostConfig() (config.Config, error) {
	path, err := config.FindInDir(s.root.Dir)
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(path)
}

// Effective returns the resolved config of app as YAML, host defaults merged, read from disk.
func (s *Store) Effective(name string) (string, error) {
	path, err := config.FindInDir(s.root.Dir)
	if err != nil {
		return "", err
	}
	root, err := config.Load(path)
	if err != nil {
		return "", fmt.Errorf("host file: %w", err)
	}
	app, err := Lookup(root, name)
	if err != nil {
		return "", err
	}
	data, err := yaml.Marshal(app.Config)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func replaceFile(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chmod(temp, info.Mode().Perm()); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

func revision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
