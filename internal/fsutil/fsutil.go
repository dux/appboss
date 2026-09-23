// Package fsutil holds the file writes every daemon-owned state file shares, and the size format
// messages about files and disks use.
package fsutil

import (
	"encoding/json"
	"fmt"
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

// HumanBytes renders a size for a log line or a message: 1023B, 1.5K, 2.0G.
func HumanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%dB", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(size)/float64(div), "KMGTPE"[exp])
}
