package super

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogWriterSealsAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.log")
	writer, err := newLogWriter(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	if _, err := writer.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	sealed, err := writer.Seal()
	if err != nil || !strings.HasSuffix(sealed, ".sealed") {
		t.Fatalf("seal returned %q, %v", sealed, err)
	}
	if data, err := os.ReadFile(sealed); err != nil || string(data) != "first\n" {
		t.Fatalf("sealed content = %q, %v", data, err)
	}
	if _, err := writer.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "second\n" {
		t.Fatalf("live file after seal = %q, %v", data, err)
	}
	second, err := writer.Seal()
	if err != nil || second == "" {
		t.Fatalf("second seal = %q, %v", second, err)
	}
	if empty, err := writer.Seal(); err != nil {
		t.Fatal(err)
	} else if empty != "" {
		t.Fatalf("empty segment should not seal, got %q", empty)
	}
}
