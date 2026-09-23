package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStaticKeys(t *testing.T) {
	dir := t.TempDir()
	defaults := Default().Defaults
	base := "procfile:\n  web:\n    command: ./server\n    hosts: [demo.test]\n"
	app, err := ParseApp([]byte(base), filepath.Join(dir, FileName), defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.WebProcesses[0].Static != DefaultStatic || !slices.Contains(app.Web.StaticExtensions, "css") || slices.Contains(app.Web.StaticExtensions, "html") {
		t.Fatalf("unexpected web defaults: %+v", app.WebProcesses[0])
	}
	custom, err := ParseApp([]byte(base+"    static: /srv/assets\nstatic_extensions: []\n"), filepath.Join(dir, FileName), defaults)
	if err != nil {
		t.Fatal(err)
	}
	if custom.WebProcesses[0].Static != "/srv/assets" || len(custom.Web.StaticExtensions) != 0 {
		t.Fatalf("app keys did not override the defaults: %+v", custom.WebProcesses[0])
	}
	disabled, err := ParseApp([]byte(base+"    static: false\n"), filepath.Join(dir, FileName), defaults)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.WebProcesses[0].Static != "" {
		t.Fatalf("static: false should disable serving: %q", disabled.WebProcesses[0].Static)
	}
	truthy, err := ParseApp([]byte(base+"    static: true\n"), filepath.Join(dir, FileName), defaults)
	if err != nil || truthy.WebProcesses[0].Static != DefaultStatic {
		t.Fatalf("static: true = %q, %v", truthy.WebProcesses[0].Static, err)
	}
	for _, bad := range []string{".css", "CSS", "tar.gz", "a/b", ""} {
		if _, err := ParseApp([]byte(base+"static_extensions: [\""+bad+"\"]\n"), filepath.Join(dir, FileName), defaults); err == nil || !strings.Contains(err.Error(), "static_extensions") {
			t.Fatalf("static_extensions %q should be rejected, got %v", bad, err)
		}
	}
}
