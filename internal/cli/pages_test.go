package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/config"
)

func TestPagesListsAndDumps(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, config.FileName)
	writeFile(t, host, "apps: ./apps\n")
	if err := os.MkdirAll(filepath.Join(dir, "apps", "shop"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "apps", "shop", config.FileName), "procfile:\n  web: ./server\n")
	run := func(args ...string) (int, string) {
		var out strings.Builder
		code := (CLI{Out: &out, Err: &out}).Run(append([]string{"pages", "-c", host}, args...))
		return code, out.String()
	}

	code, out := run("shop")
	if code != 0 || !strings.Contains(out, "maintenance") || !strings.Contains(out, "built-in") || strings.Contains(out, "\n404 ") {
		t.Fatalf("list shop: %d %s", code, out)
	}
	if code, out := run(); code != 0 || !strings.Contains(out, "404") || !strings.Contains(out, "login") {
		t.Fatalf("list host: %d %s", code, out)
	}

	appPages := filepath.Join(dir, "apps", "shop", "public", "error_pages")
	if code, out := run("dump", "shop"); code != 0 || !strings.Contains(out, filepath.Join(appPages, "template.html")) {
		t.Fatalf("dump template: %d %s", code, out)
	}
	if code, out := run("dump", "shop", "error"); code != 0 {
		t.Fatalf("dump error: %d %s", code, out)
	}
	data, err := os.ReadFile(filepath.Join(appPages, "error.html"))
	if err != nil || !strings.Contains(string(data), "Something went wrong") {
		t.Fatalf("error.html = %q, %v", data, err)
	}
	if code, out := run("shop"); code != 0 || !strings.Contains(out, filepath.Join(appPages, "error.html")) || !strings.Contains(out, filepath.Join(appPages, "template.html")) {
		t.Fatalf("list after dump: %d %s", code, out)
	}
	if code, out := run("dump", "shop", "error"); code != 1 || !strings.Contains(out, "use --force") {
		t.Fatalf("second dump must keep the file: %d %s", code, out)
	}
	if code, _ := run("dump", "shop", "error", "--force"); code != 0 {
		t.Fatal("--force must overwrite")
	}
	if code, out := run("dump", "shop", "404"); code == 0 || !strings.Contains(out, "host page") {
		t.Fatalf("a host page under an app: %d %s", code, out)
	}
	if code, out := run("dump", "--all"); code != 0 || !strings.Contains(out, filepath.Join(dir, "public", "error_pages", "login.html")) {
		t.Fatalf("dump host --all: %d %s", code, out)
	}
	if code := (CLI{Out: io.Discard, Err: io.Discard}).Run([]string{"pages", "-c", host, "dump", "nope"}); code == 0 {
		t.Fatal("an unknown app or page must fail")
	}
}
