package console

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"dboss/internal/config"
)

// Every nav tab must resolve in viewFromHash or its section never renders.
// The #help tab silently lost its case once, so guard the pairing from here.
func TestEveryConsoleTabIsRouted(t *testing.T) {
	data, err := assets.ReadFile("static/fez/db-shell.fez")
	if err != nil {
		t.Fatal(err)
	}
	shell := string(data)
	tabs := regexp.MustCompile(`href="#([a-z]+)"`).FindAllStringSubmatch(shell, -1)
	if len(tabs) == 0 {
		t.Fatal("no nav tabs found in db-shell.fez")
	}
	for _, tab := range tabs {
		view := tab[1]
		if view == "overview" {
			continue // the fallback view
		}
		if !strings.Contains(shell, "location.hash === '#"+view+"'") {
			t.Errorf("nav tab #%s has no viewFromHash case", view)
		}
	}
}

// The visual config form is a child of db-config, so index.html must load it before db-config.
func TestConfigFormComponentIsLoaded(t *testing.T) {
	index, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	form := strings.Index(html, `fez="/assets/fez/db-config-form.fez"`)
	parent := strings.Index(html, `fez="/assets/fez/db-config.fez"`)
	if form < 0 || parent < 0 || form > parent {
		t.Error("index.html must load db-config-form.fez before db-config.fez")
	}
	if _, err := assets.ReadFile("static/fez/db-config-form.fez"); err != nil {
		t.Fatalf("db-config-form.fez is not embedded: %v", err)
	}
}

// Help feature pages list their config keys by path and render them from the registry,
// so a renamed or removed key must fail here instead of silently vanishing from the page.
func TestHelpKeyPathsExist(t *testing.T) {
	data, err := assets.ReadFile("static/fez/db-help.fez")
	if err != nil {
		t.Fatal(err)
	}
	lists := regexp.MustCompile(`<db-config-keys paths="([^"]+)"`).FindAllStringSubmatch(string(data), -1)
	if len(lists) == 0 {
		t.Fatal("no feature key lists found in db-help.fez")
	}
	known := config.KeyPaths()
	for _, list := range lists {
		for _, path := range strings.Split(list[1], ",") {
			if !slices.Contains(known, path) {
				t.Errorf("db-help.fez lists unknown config key %q", path)
			}
		}
	}
}
