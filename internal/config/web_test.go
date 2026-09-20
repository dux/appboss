package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStaticAndErrorPageKeys(t *testing.T) {
	dir := t.TempDir()
	defaults := Default().Defaults
	base := "procfile:\n  web: ./server\nhosts: [demo.test]\n"
	app, err := ParseApp([]byte(base), filepath.Join(dir, FileName), defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.Web.Static != "./public" || !slices.Contains(app.Web.StaticExtensions, "css") || slices.Contains(app.Web.StaticExtensions, "html") || app.Web.ErrorPagePath != "" {
		t.Fatalf("unexpected web defaults: %+v", app.Web)
	}
	custom, err := ParseApp([]byte(base+"static: \"\"\nstatic_extensions: []\nerror_page_path: public/error_500.html\n"), filepath.Join(dir, FileName), defaults)
	if err != nil {
		t.Fatal(err)
	}
	if custom.Web.Static != "" || len(custom.Web.StaticExtensions) != 0 || custom.Web.ErrorPagePath != "public/error_500.html" {
		t.Fatalf("app keys did not override the defaults: %+v", custom.Web)
	}
	for _, bad := range []string{".css", "CSS", "tar.gz", "a/b", ""} {
		if _, err := ParseApp([]byte(base+"static_extensions: [\""+bad+"\"]\n"), filepath.Join(dir, FileName), defaults); err == nil || !strings.Contains(err.Error(), "static_extensions") {
			t.Fatalf("static_extensions %q should be rejected, got %v", bad, err)
		}
	}
}
