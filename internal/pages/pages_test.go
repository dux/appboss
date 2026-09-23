package pages

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLookupOrder(t *testing.T) {
	app, host := t.TempDir(), t.TempDir()
	if path, data := Lookup(Maintenance, app, host); path != "" || data != nil {
		t.Fatalf("empty folders found %q", path)
	}
	write(t, filepath.Join(host, TemplateFile), "host template")
	write(t, filepath.Join(host, "maintenance.html"), "host page")
	if path, _ := Lookup(Maintenance, app, host); path != filepath.Join(host, "maintenance.html") {
		t.Fatalf("host page = %q", path)
	}
	write(t, filepath.Join(app, TemplateFile), "app template")
	if path, _ := Lookup(Maintenance, app, host); path != filepath.Join(app, TemplateFile) {
		t.Fatalf("the app template must win over the host: %q", path)
	}
	write(t, filepath.Join(app, "maintenance.html"), "app page")
	if path, _ := Lookup(Maintenance, "", app, host); path != filepath.Join(app, "maintenance.html") {
		t.Fatalf("the app page must win: %q", path)
	}
}

func TestRenderFillsKnownTokensOnly(t *testing.T) {
	page := Page{Name: Starting, App: "<shop>", Action: `<a href="/">go</a>`}
	got := string(page.Fill([]byte("{{status}}|{{title}}|{{message}}|{{action}}|{{app}}|{{dboss_logo}}|{{mine}}")))
	want := `503|&lt;shop&gt; is starting|It will be ready in a few seconds. This page reloads on its own.|<a href="/">go</a>|&lt;shop&gt;|/.well-known/dboss/logo.svg|{{mine}}`
	if got != want {
		t.Fatalf("filled\n%s\nwant\n%s", got, want)
	}
	if got := string(Page{Name: Error, App: "shop", Status: 504}.Fill([]byte("{{status}}"))); got != "504" {
		t.Fatalf("status override = %q", got)
	}
	if got := string(Page{Name: NotFound}.Fill([]byte("{{title}}|{{app}}"))); got != "Nothing here|" {
		t.Fatalf("host page = %q", got)
	}
}

func TestBuiltInTemplateRendersEveryPage(t *testing.T) {
	for _, spec := range Specs {
		body := string(Page{Name: spec.Name, App: "shop"}.Render())
		if strings.Contains(body, "{{") || !strings.Contains(body, LogoPath) {
			t.Errorf("%s left a token or lost the logo:\n%s", spec.Name, body)
		}
	}
	response := httptest.NewRecorder()
	Page{Name: Crashed, App: "shop"}.Write(response)
	if response.Code != 503 || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), "shop is not running") {
		t.Fatalf("write = %d %v", response.Code, response.Header())
	}
}

func TestSourceWritesTheWordingIn(t *testing.T) {
	source := string(Source(Starting))
	if !strings.Contains(source, "{{app}} is starting") || strings.Contains(source, "{{title}}") || !strings.Contains(source, "{{action}}") {
		t.Fatalf("source = %s", source)
	}
	if string(Source("")) != string(Template) {
		t.Fatal("an empty name is the template itself")
	}
}
