package devtls

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Install adds the root to the system trust store: the login keychain on macOS (the keychain
// asks for the password), the system store through sudo on Debian and Fedora families. On any
// other system it returns an error naming the file to import by hand.
func (a *Authority) Install(out io.Writer) error {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		keychain := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
		return run(out, "security", "add-trusted-cert", "-r", "trustRoot", "-k", keychain, a.RootPath())
	case "linux":
		if dir := "/usr/local/share/ca-certificates"; isDir(dir) {
			if err := run(out, "sudo", "install", "-m", "0644", a.RootPath(), filepath.Join(dir, "dboss-dev.crt")); err != nil {
				return err
			}
			return run(out, "sudo", "update-ca-certificates")
		}
		if dir := "/etc/pki/ca-trust/source/anchors"; isDir(dir) {
			if err := run(out, "sudo", "install", "-m", "0644", a.RootPath(), filepath.Join(dir, "dboss-dev.pem")); err != nil {
				return err
			}
			return run(out, "sudo", "update-ca-trust", "extract")
		}
	}
	return fmt.Errorf("no known trust store on %s; import %s into your system or browser by hand", runtime.GOOS, a.RootPath())
}

func run(out io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
