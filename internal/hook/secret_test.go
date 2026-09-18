package hook

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateIsSixtyFourChars(t *testing.T) {
	secret, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 64 {
		t.Fatalf("secret length = %d (%q)", len(secret), secret)
	}
	other, _ := Generate()
	if secret == other {
		t.Fatal("two generated secrets are equal")
	}
}

func TestEnsurePersistsAndRotateReplaces(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Ensure("app", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Get("app", "deploy"); got != first {
		t.Fatalf("Get = %q, want %q", got, first)
	}
	// A fresh open sees the persisted value.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get("app", "deploy"); got != first {
		t.Fatalf("reopened Get = %q, want %q", got, first)
	}
	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	if err := store.Set("app", "deploy", "replacement"); err != nil {
		t.Fatal(err)
	}
	if got := store.Get("app", "deploy"); got != "replacement" {
		t.Fatalf("after Set Get = %q", got)
	}
}

func TestReconcileDropsRemovedHooks(t *testing.T) {
	dir := t.TempDir()
	store, _ := Open(dir)
	_, _ = store.Ensure("app", "deploy")
	_, _ = store.Ensure("app", "old")
	_, _ = store.Ensure("gone", "deploy")
	if err := store.Reconcile(map[string]map[string]bool{"app": {"deploy": true}}); err != nil {
		t.Fatal(err)
	}
	if store.Get("app", "old") != "" {
		t.Error("removed hook kept its secret")
	}
	if store.Get("app", "deploy") == "" {
		t.Error("live hook lost its secret")
	}
	if store.Get("gone", "deploy") != "" {
		t.Error("removed app kept its secret")
	}
}
