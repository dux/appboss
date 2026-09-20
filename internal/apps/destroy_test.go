package apps

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDestroyRemovesDirectoryAndDoesNotFollowSymlink(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plain, "app"), []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Destroy(root, "plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(plain); !os.IsNotExist(err) {
		t.Fatalf("plain app still exists: %v", err)
	}

	target := filepath.Join(t.TempDir(), "release")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Destroy(root, "linked"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("app symlink still exists: %v", err)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("symlink target was removed: %v", err)
	}
}

func TestDestroyRejectsPathsOutsideAppsRoot(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"", ".", "..", "nested/app", "/tmp/app"} {
		if err := Destroy(root, name); err == nil {
			t.Errorf("Destroy(%q) should fail", name)
		}
	}
}
