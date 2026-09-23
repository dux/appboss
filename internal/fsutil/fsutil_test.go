package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteJSONRoundTripsWithExactMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	if err := WriteJSON(path, map[string]int{"a": 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	var got map[string]int
	if err := ReadJSON(path, &got); err != nil || got["a"] != 1 {
		t.Fatalf("ReadJSON = %v, %v", got, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}

func TestReadJSONMissingFileIsEmpty(t *testing.T) {
	got := map[string]int{"kept": 1}
	if err := ReadJSON(filepath.Join(t.TempDir(), "missing.json"), &got); err != nil {
		t.Fatal(err)
	}
	if got["kept"] != 1 {
		t.Fatal("missing file changed the value")
	}
}

func TestReadJSONRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	if err := ReadJSON(path, &got); err == nil {
		t.Fatal("corrupt file read without error")
	}
}
