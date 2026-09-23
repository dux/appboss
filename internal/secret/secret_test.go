package secret

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
	path := filepath.Join(t.TempDir(), "hook-secrets.json")
	store, err := Open(path)
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
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get("app", "deploy"); got != first {
		t.Fatalf("reopened Get = %q, want %q", got, first)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	rotated, err := store.Rotate("app", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if rotated == first || store.Get("app", "deploy") != rotated {
		t.Fatalf("after Rotate Get = %q, rotated %q, first %q", store.Get("app", "deploy"), rotated, first)
	}
}

func TestReconcileDropsRemovedHooks(t *testing.T) {
	store, _ := Open(filepath.Join(t.TempDir(), "hook-secrets.json"))
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

func TestOpenRefusesACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook-secrets.json")
	if err := os.WriteFile(path, []byte(`{"app/web":"flat"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("a file in the wrong shape opened as an empty store")
	}
}
