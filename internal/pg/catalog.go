package pg

import (
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"dboss/internal/fsutil"
	"dboss/internal/logx"
)

// Backup is one dump recorded in the catalog. A failed entry has no files but keeps the error so
// the console can show why a run did not land. Manual entries are never pruned.
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
	Manual     bool   `json:"manual,omitempty"`
}

// catalogKeep bounds the catalog file. Retention prunes far less than this; the cap only stops a
// long stream of failed runs from growing the file without bound.
const catalogKeep = 2000

// catalog is the daemon-written list of dumps under state_dir/pg-backups.json. A file that fails
// to load stays the source of truth: loadErr blocks every write, so the next dump cannot replace
// the whole history with one entry.
type catalog struct {
	mu      sync.Mutex
	path    string
	entries []Backup
	loaded  bool
	loadErr error
}

func newCatalog(stateDir string) *catalog {
	return &catalog{path: filepath.Join(stateDir, "pg-backups.json")}
}

func (c *catalog) load() error {
	if c.loaded {
		return c.loadErr
	}
	c.loaded = true
	if err := fsutil.ReadJSON(c.path, &c.entries); err != nil {
		c.entries = nil
		c.loadErr = fmt.Errorf("backup catalog %s: %w", c.path, err)
		logx.Warnf("postgres: %v", c.loadErr)
	}
	return c.loadErr
}

// list returns the entries newest first.
func (c *catalog) list() []Backup {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.load()
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
	if err := c.load(); err != nil {
		return err
	}
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
	if err := c.load(); err != nil {
		return err
	}
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
	return fsutil.WriteJSON(c.path, c.entries, 0o640)
}
