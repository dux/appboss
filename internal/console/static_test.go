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

// The Logs page links to the global blocked-requests page (#/blocked), which db-shell fetches as
// tpl-blocked.fez on demand. A rename on either side would break the link silently.
func TestBlockedPageIsLinked(t *testing.T) {
	logs, err := assets.ReadFile("static/fez/tpl-logs.fez")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logs), `href="#/blocked"`) {
		t.Error("tpl-logs.fez must link to #/blocked")
	}
	if _, err := assets.ReadFile("static/fez/tpl-blocked.fez"); err != nil {
		t.Fatalf("tpl-blocked.fez is not embedded: %v", err)
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

// ui-btn is a shared widget, so it loads from index.html rather than per route. Every action
// button on the Overview cards is one, and they all vanish if the tag is never registered.
func TestButtonComponentIsLoaded(t *testing.T) {
	index, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `fez="/assets/fez/ui-btn.fez"`) {
		t.Error("index.html must load ui-btn.fez")
	}
	if _, err := assets.ReadFile("static/fez/ui-btn.fez"); err != nil {
		t.Fatalf("ui-btn.fez is not embedded: %v", err)
	}
}

// The vendored fez build must be 0.10.0 or newer. Older builds observe every attribute write on
// a component root and feed it back into props, which re-enters any component that writes its own
// root - ui-btn does - and they have no Fez(node).setAttribute, the channel the app cards use to
// show an action as pending. Re-vendoring an older bundle froze the cards until a page reload.
// The hash routes call Fez.hpath / Fez.hqs, which delegate to Fez.pjax; builds that only set
// window.Pjax lack those shortcuts.
func TestVendoredFezIsCurrent(t *testing.T) {
	bundle, err := assets.ReadFile("static/fez.min.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bundle), "onPropsChange") {
		t.Error("fez.min.js still carries onPropsChange: vendor 0.10.0 or newer")
	}
	for _, name := range []string{"hpath", "hqs"} {
		if !strings.Contains(string(bundle), ".pjax."+name+"(") {
			t.Errorf("fez.min.js has no Fez.%s: the console's hash routes need it", name)
		}
	}
}

// ui-tabs is a shared widget too: the PostgreSQL database page switches between Backup and SQL
// with it, and an unregistered tag would leave the page stuck on whatever tab it loaded with.
func TestTabsComponentIsLoaded(t *testing.T) {
	index, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `fez="/assets/fez/ui-tabs.fez"`) {
		t.Error("index.html must load ui-tabs.fez")
	}
	if _, err := assets.ReadFile("static/fez/ui-tabs.fez"); err != nil {
		t.Fatalf("ui-tabs.fez is not embedded: %v", err)
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

// Every Help nav topic must have a matching {#if state.topic === '<id>'} section, or selecting it
// renders nothing. The #help tab silently lost its case once, so guard the pairing from here.
func TestHelpTopicsHaveSections(t *testing.T) {
	data, err := assets.ReadFile("static/fez/tpl-help.fez")
	if err != nil {
		t.Fatal(err)
	}
	topics := regexp.MustCompile(`\{ id: '([a-z0-9_-]+)'`).FindAllStringSubmatch(string(data), -1)
	if len(topics) == 0 {
		t.Fatal("no help topics found in tpl-help.fez")
	}
	for _, topic := range topics {
		if !strings.Contains(string(data), "state.topic === '"+topic[1]+"'") {
			t.Errorf("help topic %q has no section", topic[1])
		}
	}
}
