package apps

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"deploy-boss/internal/config"
	"gopkg.in/yaml.v3"
)

// ErrConflict is returned by Write when the file on disk no longer matches the revision the
// caller edited. The returned ConfigFile then carries the current contents.
var ErrConflict = errors.New("file changed on disk")

// ConfigFile is one file dboss reads. IDs are "host" or "app:<name>" and map to paths only on
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
type Store struct{ root config.Config }

func NewStore(root config.Config) *Store { return &Store{root: root} }

// Files lists the host file and, in host mode, the active file of every app folder.
func (s *Store) Files() ([]ConfigFile, error) {
	files := []ConfigFile{{ID: "host", Path: s.root.SourcePath, Source: filepath.Base(s.root.SourcePath)}}
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
			return errors.New("the host file cannot switch between apps and procfile while dboss runs")
		}
		if parsed.App != nil {
			_, err = validateApp(*parsed.App)
		}
		return err
	}
	root, err := config.Load(s.root.SourcePath)
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
	if err := replaceFile(current.Path, []byte(contents)); err != nil {
		return ConfigFile{}, err
	}
	return s.Read(id)
}

// CreateLocal copies an app's dboss.yaml to dboss.local.yaml so edits made on the server live
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

// Effective returns the resolved config of app as YAML, host defaults merged, read from disk.
func (s *Store) Effective(name string) (string, error) {
	root, err := config.Load(s.root.SourcePath)
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
