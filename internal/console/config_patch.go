package console

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// applyFormPatch rewrites the dotted paths in values and removes the paths in reset, leaving
// every other key and its comments untouched. The caller has already turned blank form fields
// into reset entries, so this only sets what it is given. An unknown intermediate is an error
// rather than a silent overwrite, and a mapping emptied by a reset is pruned.
func applyFormPatch(contents string, values map[string]any, reset []string) (string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(contents), &root); err != nil {
		return "", err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("config must be a YAML mapping")
	}
	document := root.Content[0]
	for path, value := range values {
		node, err := yamlValue(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		if err := setYAMLPath(document, strings.Split(path, "."), node); err != nil {
			return "", err
		}
	}
	for _, path := range reset {
		removeYAMLPath(document, strings.Split(path, "."))
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// yamlValue converts a JSON-decoded value into a YAML node. Whole float64s become ints so a
// count or a port is written as 10, not 10.0.
func yamlValue(value any) (*yaml.Node, error) {
	node := &yaml.Node{}
	if err := node.Encode(normalizeNumber(value)); err != nil {
		return nil, err
	}
	return node, nil
}

func normalizeNumber(value any) any {
	if number, ok := value.(float64); ok && number == float64(int64(number)) {
		return int64(number)
	}
	return value
}

// setYAMLPath sets value at the dotted path under mapping, creating intermediate mappings.
func setYAMLPath(mapping *yaml.Node, path []string, value *yaml.Node) error {
	if len(path) == 0 {
		return fmt.Errorf("empty key path")
	}
	index := mappingKeyIndex(mapping, path[0])
	if len(path) == 1 {
		if index >= 0 {
			mapping.Content[index+1] = value
		} else {
			mapping.Content = append(mapping.Content, scalarNode(path[0]), value)
		}
		return nil
	}
	child := (*yaml.Node)(nil)
	if index >= 0 {
		child = mapping.Content[index+1]
		if child.Kind != yaml.MappingNode {
			return fmt.Errorf("%s is not a mapping", path[0])
		}
	} else {
		child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapping.Content = append(mapping.Content, scalarNode(path[0]), child)
	}
	return setYAMLPath(child, path[1:], value)
}

// removeYAMLPath deletes the leaf at path and prunes any parent it leaves empty, so clearing the
// last pubsub key does not leave a bare `pubsub: {}` behind.
func removeYAMLPath(mapping *yaml.Node, path []string) {
	if len(path) == 0 {
		return
	}
	index := mappingKeyIndex(mapping, path[0])
	if index < 0 {
		return
	}
	if len(path) == 1 {
		mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
		return
	}
	child := mapping.Content[index+1]
	if child.Kind != yaml.MappingNode {
		return
	}
	removeYAMLPath(child, path[1:])
	if len(child.Content) == 0 {
		mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
	}
}

func mappingKeyIndex(mapping *yaml.Node, key string) int {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func scalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}
