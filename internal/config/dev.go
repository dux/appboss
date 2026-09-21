package config

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// DevSuffix marks the development variant of a key: `listen: ":80"` next to `listen_dev: ":3000"`
// keeps both values in one file, and the session picks one at load time.
const DevSuffix = "_dev"

// Dev reports whether this is a development session: the config dboss was pointed at is an app
// (it has procfile:), so one app is being run from its own folder rather than a host serving an
// apps directory. It is the one check every dev-only behavior reads.
func (c Config) Dev() bool { return c.App != nil }

// applyDev resolves the _dev suffix across a whole document, before the schema check turns it
// into structs. In a dev session every `<key>_dev` replaces `<key>` and creates it when it is
// absent; otherwise it is dropped, so an app file carries its dev values into a host unread. The
// value replaces the base outright, so a block override names the leaf key it changes rather
// than restating the block. It reports whether the tree changed.
func applyDev(node *yaml.Node, dev bool) bool {
	changed := false
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			changed = applyDev(child, dev) || changed
		}
	case yaml.MappingNode:
		for _, child := range node.Content {
			changed = applyDev(child, dev) || changed
		}
		changed = resolveDev(node, dev) || changed
	}
	return changed
}

// resolveDev applies the suffix to one mapping node and removes every pair that still carries it.
func resolveDev(node *yaml.Node, dev bool) bool {
	overrides := []int{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if isDevKey(node.Content[i].Value) {
			overrides = append(overrides, i)
		}
	}
	if len(overrides) == 0 {
		return false
	}
	if dev {
		at := map[string]int{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			at[node.Content[i].Value] = i
		}
		for _, i := range overrides {
			base := strings.TrimSuffix(node.Content[i].Value, DevSuffix)
			if j, ok := at[base]; ok {
				node.Content[j+1] = node.Content[i+1]
				continue
			}
			// No base key to override: rename the pair in place so it keeps its position.
			node.Content[i].Value = base
			at[base] = i
		}
	}
	kept := node.Content[:0]
	for i := 0; i+1 < len(node.Content); i += 2 {
		if isDevKey(node.Content[i].Value) {
			continue
		}
		kept = append(kept, node.Content[i], node.Content[i+1])
	}
	node.Content = kept
	return true
}

func isDevKey(name string) bool {
	return len(name) > len(DevSuffix) && strings.HasSuffix(name, DevSuffix)
}

// hasTopKey reports whether the document's root mapping has name, so dev mode can be read off the
// tree before it is decoded.
func hasTopKey(root *yaml.Node, name string) bool {
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return false
	}
	content := root.Content[0].Content
	for i := 0; i+1 < len(content); i += 2 {
		if content[i].Value == name {
			return true
		}
	}
	return false
}
