package pg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Backup is one dump recorded in the catalog. A failed entry has no files but keeps the error so
// the console can show why a run did not land.
type Backup struct {
	ID         string `json:"id"`
	Database   string `json:"database"`
	Time       string `json:"time"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256,omitempty"`
	LocalPath  string `json:"local_path,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Globals    bool   `json:"globals,omitempty"`
	Verified   bool   `json:"verified,omitempty"`
}

// catalogKeep bounds the catalog file. Retention prunes far less than this; the cap only stops a
// long stream of failed runs from growing the file without bound.
const catalogKeep = 2000

// catalog is the daemon-written list of dumps under state_dir/pg-backups.json.
type catalog struct {
	mu      sync.Mutex
	path    string
	entries []Backup
	loaded  bool
}

func newCatalog(stateDir string) *catalog {
	return &catalog{path: filepath.Join(stateDir, "pg-backups.json")}
}

func (c *catalog) load() {
	if c.loaded {
		return
	}
	c.loaded = true
	data, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &c.entries)
}

// list returns the entries newest first.
func (c *catalog) list() []Backup {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	result := append([]Backup(nil), c.entries...)
	sort.Slice(result, func(i, j int) bool { return result[i].Time > result[j].Time })
	return result
}

func (c *catalog) get(id string) (Backup, bool) {
	for _, entry := range c.list() {
		if entry.ID == id {
			return entry, true
		}
	}
	return Backup{}, false
}

func (c *catalog) record(entry Backup) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	c.entries = append(c.entries, entry)
	if len(c.entries) > catalogKeep {
		c.entries = c.entries[len(c.entries)-catalogKeep:]
	}
	return c.save()
}

// forget drops the entries by id, used after retention removed their files.
func (c *catalog) forget(ids map[string]bool) error {
	if len(ids) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	kept := c.entries[:0]
	for _, entry := range c.entries {
		if !ids[entry.ID] {
			kept = append(kept, entry)
		}
	}
	c.entries = kept
	return c.save()
}

// save writes the catalog atomically.
func (c *catalog) save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return err
	}
	temp := c.path + ".tmp"
	if err := os.WriteFile(temp, append(data, '\n'), 0o640); err != nil {
		return err
	}
	return os.Rename(temp, c.path)
}
