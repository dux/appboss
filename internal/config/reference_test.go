package config

import (
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestReferenceCoversKeys keeps the hand-written reference in sync with the registry: every key
// Keys() documents must appear somewhere in reference.yaml. The walk collects nested paths, so
// proxy.wake.retry_after is matched in place, and strips a leading defaults. because the shared
// keys sit under defaults: in part 1 and at the top level of the app part.
func TestReferenceCoversKeys(t *testing.T) {
	present := map[string]bool{}
	decoder := yaml.NewDecoder(strings.NewReader(Reference))
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reference.yaml does not parse: %v", err)
		}
		collectYAMLPaths(&document, "", present)
	}
	missing := []string{}
	for _, key := range Keys() {
		if !present[key.Path] && !present["defaults."+key.Path] {
			missing = append(missing, key.Path)
		}
	}
	if len(missing) > 0 {
		t.Errorf("reference.yaml is missing %d key(s): %s", len(missing), strings.Join(missing, ", "))
	}
}

func collectYAMLPaths(node *yaml.Node, prefix string, paths map[string]bool) {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		collectYAMLPaths(node.Content[0], prefix, paths)
		return
	}
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		path := node.Content[i].Value
		if prefix != "" {
			path = prefix + "." + path
		}
		paths[path] = true
		collectYAMLPaths(node.Content[i+1], path, paths)
	}
}
