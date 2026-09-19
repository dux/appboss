package console

import (
	"regexp"
	"strings"
	"testing"
)

// Every nav tab must resolve in viewFromHash or its section never renders.
// The #help tab silently lost its case once, so guard the pairing from here.
func TestEveryConsoleTabIsRouted(t *testing.T) {
	data, err := assets.ReadFile("static/fez/ab-shell.fez")
	if err != nil {
		t.Fatal(err)
	}
	shell := string(data)
	tabs := regexp.MustCompile(`href="#([a-z]+)"`).FindAllStringSubmatch(shell, -1)
	if len(tabs) == 0 {
		t.Fatal("no nav tabs found in ab-shell.fez")
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

// The visual config form is a child of ab-config, so index.html must load it before ab-config.
func TestConfigFormComponentIsLoaded(t *testing.T) {
	index, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	form := strings.Index(html, `fez="/assets/fez/ab-config-form.fez"`)
	parent := strings.Index(html, `fez="/assets/fez/ab-config.fez"`)
	if form < 0 || parent < 0 || form > parent {
		t.Error("index.html must load ab-config-form.fez before ab-config.fez")
	}
	if _, err := assets.ReadFile("static/fez/ab-config-form.fez"); err != nil {
		t.Fatalf("ab-config-form.fez is not embedded: %v", err)
	}
}
