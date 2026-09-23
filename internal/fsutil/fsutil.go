// Package fsutil holds the file writes every daemon-owned state file shares.
package fsutil

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// WriteFile replaces path with data atomically: a crash leaves either the old file or the new
// one, never half of it. The parent directory is created when missing, and perm is applied
// exactly, whatever the umask.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, perm); err != nil {
		return err
	}
	if err := os.Chmod(temp, perm); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// WriteJSON stores value as indented JSON through WriteFile.
func WriteJSON(path string, value any, perm os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, append(data, '\n'), perm)
}

// ReadJSON decodes path into value. A missing file leaves value untouched and is not an error.
func ReadJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
