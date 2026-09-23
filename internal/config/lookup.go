package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FileName and LocalFileName are the two config file names looked up in a folder.
// The local file is server-only and, when present, replaces the committed one entirely.
// ConfigDir is the subfolder searched when the folder itself has neither, so an app can keep
// its dboss.yaml under config/ next to the rest of its configuration.
const (
	FileName      = "dboss.yaml"
	LocalFileName = "dboss.local.yaml"
	ConfigDir     = "config"
)

// ErrNoConfig is FindInDir's answer for a folder with no config file in either place.
var ErrNoConfig = errors.New("no config file")

// FindInDir returns the config file to use for dir: dboss.local.yaml, else dboss.yaml, looked up
// in dir and then in dir/config. Files in both places are an error, so only one can be live.
func FindInDir(dir string) (string, error) {
	nestedDir := filepath.Join(dir, ConfigDir)
	root, nested := findConfigFile(dir), findConfigFile(nestedDir)
	switch {
	case root != "" && nested != "":
		return "", fmt.Errorf("both %s and %s exist; keep one", root, nested)
	case root != "":
		return root, nil
	case nested != "":
		return nested, nil
	}
	return "", fmt.Errorf("%w: no %s in %s or %s", ErrNoConfig, FileName, dir, nestedDir)
}

// Live is the file that holds path's config right now: a dboss.local.yaml created next to a
// dboss.yaml after start replaces it, and a removed one hands back to the base. A file with any
// other name is used as given.
func Live(path string) string {
	if name := filepath.Base(path); name != FileName && name != LocalFileName {
		return path
	}
	if found := findConfigFile(filepath.Dir(path)); found != "" {
		return found
	}
	return path
}

func findConfigFile(dir string) string {
	for _, name := range []string{LocalFileName, FileName} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// BaseDir is the folder a config file belongs to, which relative paths resolve against: the
// parent of config/ when that is where FindInDir found it, else the file's own folder.
func BaseDir(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) != ConfigDir {
		return dir
	}
	parent := filepath.Dir(dir)
	if found, err := FindInDir(parent); err == nil && found == path {
		return parent
	}
	return dir
}
