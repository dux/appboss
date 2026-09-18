package apps

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"deploy-boss/internal/config"
)

func storeFixture(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "sinatra")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(appDir, config.FileName), "procfile:\n  web: ./server\nhosts: [sinatra.test]\n")
	writeTestFile(t, filepath.Join(root, config.FileName), "apps: ./apps\ndefaults:\n  idle_stop: 1h\n")
	cfg, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(cfg), root
}

func TestStoreListsReadsAndWritesRealFiles(t *testing.T) {
	store, root := storeFixture(t)
	files, err := store.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].ID != "host" || files[1].ID != "app:sinatra" || files[1].Source != config.FileName || files[1].HasLocal || files[1].Contents != "" {
		t.Fatalf("unexpected files: %+v", files)
	}
	file, err := store.Read("app:sinatra")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(file.Contents, "sinatra.test") || file.Revision != files[1].Revision {
		t.Fatalf("unexpected read: %+v", file)
	}
	if err := store.Validate("app:sinatra", "procfile:\n  web: ./server\nproxy:\n  listen: :80\n"); err == nil || !strings.Contains(err.Error(), "only valid in the root") {
		t.Fatalf("host key in app file: %v", err)
	}
	if err := store.Validate("app:sinatra", "procfile:\n  web: ./server\nhosts: [sinatra.test]\nweb_process: api\n"); err == nil || !strings.Contains(err.Error(), "web_process") {
		t.Fatalf("procfile rules must apply: %v", err)
	}
	if err := store.Validate("host", "procfile:\n  web: ./server\n"); err == nil || !strings.Contains(err.Error(), "cannot switch") {
		t.Fatalf("role change must be rejected: %v", err)
	}
	if err := store.Validate("host", "apps: ./apps\ndefaults:\n  nope: 1\n"); err == nil {
		t.Fatal("unknown key must fail validation")
	}
	if _, err := store.Write("app:sinatra", "procfile:\n  web: ./server\nhosts: [sinatra.test, www.sinatra.test]\n", "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	if err := os.Chmod(file.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	written, err := store.Write("app:sinatra", "procfile:\n  web: ./server\nhosts: [sinatra.test, www.sinatra.test]\n", file.Revision)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "apps", "sinatra", config.FileName))
	info, _ := os.Stat(filepath.Join(root, "apps", "sinatra", config.FileName))
	if !strings.Contains(string(data), "www.sinatra.test") || written.Revision == file.Revision || info.Mode().Perm() != 0o600 {
		t.Fatalf("write did not land: %q mode=%v", data, info.Mode())
	}
	if _, err := os.Stat(file.Path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temp file left behind")
	}
	effective, err := store.Effective("sinatra")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(effective, "idle_stop: 1h0m0s") || !strings.Contains(effective, "www.sinatra.test") {
		t.Fatalf("effective config should merge host defaults: %s", effective)
	}
}

func TestStoreCreatesServerOverride(t *testing.T) {
	store, root := storeFixture(t)
	file, err := store.CreateLocal("sinatra")
	if err != nil {
		t.Fatal(err)
	}
	if file.Source != config.LocalFileName || !file.HasLocal || file.Path != filepath.Join(root, "apps", "sinatra", config.LocalFileName) || !strings.Contains(file.Contents, "sinatra.test") {
		t.Fatalf("unexpected local file: %+v", file)
	}
	if _, err := store.CreateLocal("sinatra"); err == nil {
		t.Fatal("second override must fail")
	}
	files, _ := store.Files()
	if files[1].Source != config.LocalFileName {
		t.Fatalf("local file should now be active: %+v", files[1])
	}
}

func TestStoreSingleModeHasOneEntry(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, config.FileName), "procfile:\n  web: ./server\n")
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(cfg)
	files, err := store.Files()
	if err != nil || len(files) != 1 || files[0].ID != "host" || files[0].App != filepath.Base(dir) {
		t.Fatalf("files = %+v, %v", files, err)
	}
	if err := store.Validate("host", "apps: ./apps\n"); err == nil {
		t.Fatal("single mode host must keep its procfile")
	}
	effective, err := store.Effective(filepath.Base(dir))
	if err != nil || !strings.Contains(effective, "web: ./server") {
		t.Fatalf("effective = %q, %v", effective, err)
	}
}
