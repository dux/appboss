package console

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"dboss/internal/config"
)

// Every nav route must resolve to a tpl-<name>.fez in db-shell.fez, or its section never renders.
// The #help tab silently lost its case once, so guard the pairing from here.
func TestEveryConsoleRouteHasTemplate(t *testing.T) {
	data, err := assets.ReadFile("static/fez/db-shell.fez")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`\{\s*name:\s*'([a-z0-9-]+)'`).FindAllStringSubmatch(string(data), -1)
	if len(routes) == 0 {
		t.Fatal("no routes found in db-shell.fez")
	}
	for _, route := range routes {
		path := "static/fez/tpl-" + route[1] + ".fez"
		if _, err := assets.ReadFile(path); err != nil {
			t.Errorf("route %q has no %s", route[1], path)
		}
	}
}

// db-config-form is preloaded in index.html while tpl-config is fetched per route, so the form
// is always compiled before the page that uses it.
func TestConfigFormComponentIsLoaded(t *testing.T) {
	index, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `fez="/assets/fez/db-config-form.fez"`) {
		t.Error("index.html must load db-config-form.fez")
	}
	if _, err := assets.ReadFile("static/fez/tpl-config.fez"); err != nil {
		t.Fatalf("tpl-config.fez is not embedded: %v", err)
	}
}

// Help feature pages list their config keys by path and render them from the registry,
// so a renamed or removed key must fail here instead of silently vanishing from the page.
func TestHelpKeyPathsExist(t *testing.T) {
	data, err := assets.ReadFile("static/fez/tpl-help.fez")
	if err != nil {
		t.Fatal(err)
	}
	lists := regexp.MustCompile(`<db-config-keys paths="([^"]+)"`).FindAllStringSubmatch(string(data), -1)
	if len(lists) == 0 {
		t.Fatal("no feature key lists found in tpl-help.fez")
	}
	known := config.KeyPaths()
	for _, list := range lists {
		for _, path := range strings.Split(list[1], ",") {
			if !slices.Contains(known, path) {
				t.Errorf("tpl-help.fez lists unknown config key %q", path)
			}
		}
	}
}
