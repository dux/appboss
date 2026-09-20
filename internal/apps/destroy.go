package apps

import (
	"fmt"
	"os"
	"path/filepath"
)

// Destroy removes one direct entry of root. RemoveAll does not follow a symlink passed as its
// root, so a deploy checkout behind an app symlink remains owned by the deploy system.
func Destroy(root, name string) error {
	if name == "" || name == "." || filepath.Base(name) != name || !filepath.IsLocal(name) {
		return fmt.Errorf("invalid app name %q", name)
	}
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("app path %q is not a directory or symlink", path)
	}
	return os.RemoveAll(path)
}
