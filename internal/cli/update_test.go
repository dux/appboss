package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"dboss/internal/version"
)

// releaseServer serves one release: the API answer, the asset and a checksums.txt whose hash
// is whatever corrupt says it should be.
func releaseServer(t *testing.T, tag string, payload []byte, corrupt bool) *httptest.Server {
	t.Helper()
	asset := "dboss_" + runtime.GOOS + "_" + runtime.GOARCH
	sum := sha256.Sum256(payload)
	listed := hex.EncodeToString(sum[:])
	if corrupt {
		listed = strings.Repeat("0", len(listed))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + releaseRepo + "/releases/latest":
			fmt.Fprintf(w, `{"tag_name": %q}`, tag)
		case "/" + releaseRepo + "/releases/download/" + tag + "/checksums.txt":
			fmt.Fprintf(w, "%s  %s\n", listed, asset)
		case "/" + releaseRepo + "/releases/download/" + tag + "/" + asset:
			w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	previousAPI, previousDownload, previousVersion := releaseAPIBase, releaseDownload, version.Version
	releaseAPIBase, releaseDownload = server.URL, server.URL
	t.Cleanup(func() {
		releaseAPIBase, releaseDownload, version.Version = previousAPI, previousDownload, previousVersion
	})
	return server
}

// fakeBinary stands in for the running executable, so installRelease has a real path to
// replace without touching the test binary.
func fakeBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dboss")
	if err := os.WriteFile(path, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUpdateInstallsRelease(t *testing.T) {
	releaseServer(t, "v84", []byte("new binary"), false)
	target := fakeBinary(t)

	var out strings.Builder
	if err := (CLI{Out: &out}).installRelease(target, "v84", false); err != nil {
		t.Fatalf("installRelease: %v", err)
	}
	installed, err := os.ReadFile(target)
	if err != nil || string(installed) != "new binary" {
		t.Fatalf("binary not replaced: %q %v", installed, err)
	}
	if !strings.Contains(out.String(), "sha256 ok") {
		t.Fatalf("output does not confirm the checksum: %q", out.String())
	}
	// The staging directory lives next to the binary and must not survive the install.
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("staging left behind: %v", entries)
	}
}

func TestUpdateRefusesBadChecksum(t *testing.T) {
	releaseServer(t, "v84", []byte("new binary"), true)
	target := fakeBinary(t)

	var out strings.Builder
	err := (CLI{Out: &out}).installRelease(target, "v84", false)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("bad checksum accepted: %v", err)
	}
	installed, readErr := os.ReadFile(target)
	if readErr != nil || string(installed) != "old binary" {
		t.Fatalf("binary was touched: %q %v", installed, readErr)
	}
}

func TestUpdateReportsLatest(t *testing.T) {
	releaseServer(t, "v84", []byte("new binary"), false)
	version.Version = "v84"

	var out, errOut strings.Builder
	if err := (CLI{Out: &out, Err: &errOut}).update(nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(out.String(), "dboss v84 is the latest release") || strings.Contains(out.String(), "downloading") {
		t.Fatalf("a current binary was not left alone: %q", out.String())
	}
}

func TestUpdateCheckNeverWrites(t *testing.T) {
	releaseServer(t, "v84", []byte("new binary"), false)
	version.Version = "v81"

	var out, errOut strings.Builder
	if err := (CLI{Out: &out, Err: &errOut}).update([]string{"--check"}); err != nil {
		t.Fatalf("update --check: %v", err)
	}
	if !strings.Contains(out.String(), "latest release is v84, you are on v81") || strings.Contains(out.String(), "downloading") {
		t.Fatalf("--check did more than report: %q", out.String())
	}

	out.Reset()
	if err := (CLI{Out: &out, Err: &errOut}).update([]string{"--check", "--json"}); err != nil {
		t.Fatalf("update --check --json: %v", err)
	}
	var result updateResult
	if err := json.Unmarshal([]byte(out.String()), &result); err != nil {
		t.Fatalf("--json is not JSON: %v %q", err, out.String())
	}
	if result.Current != "v81" || result.Latest != "v84" || result.Updated {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestUpdateRefusesSourceBuild(t *testing.T) {
	releaseServer(t, "v84", []byte("new binary"), false)
	version.Version = version.Dev

	var out, errOut strings.Builder
	err := (CLI{Out: &out, Err: &errOut}).update(nil)
	if err == nil || !strings.Contains(err.Error(), "make build") {
		t.Fatalf("a dev build was replaced: %v", err)
	}
}

func TestReleaseNumber(t *testing.T) {
	for _, item := range []struct {
		tag    string
		number int
		ok     bool
	}{
		{"v81", 81, true},
		{"v0", 0, true},
		{"dev", 0, false},
		{"81", 0, false},
		{"v0.1.0", 0, false},
		{"", 0, false},
	} {
		number, ok := releaseNumber(item.tag)
		if number != item.number || ok != item.ok {
			t.Errorf("releaseNumber(%q) = %d, %v; want %d, %v", item.tag, number, ok, item.number, item.ok)
		}
	}
}

func TestChecksumFor(t *testing.T) {
	listing := "aaa  dboss_linux_amd64\nbbb  dboss_darwin_arm64\n"
	if got := checksumFor(listing, "dboss_darwin_arm64"); got != "bbb" {
		t.Errorf("checksumFor = %q", got)
	}
	if got := checksumFor(listing, "dboss_windows_amd64"); got != "" {
		t.Errorf("unknown asset returned %q", got)
	}
}
