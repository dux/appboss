package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// checkKeys walks the document against the struct schema and reports the first unknown key,
// before the strict decoder does, so the message names the section and suggests the closest key.
func checkKeys(node *yaml.Node, schema reflect.Type, prefix string) *Error {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	fields := schemaFields(schema)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valueNode := node.Content[i], node.Content[i+1]
		field, ok := fields[keyNode.Value]
		if !ok {
			// A _dev variant is checked as the key it overrides, so a typo in a value that only a
			// dev session reads is still caught by dboss check on a host.
			if base, found := strings.CutSuffix(keyNode.Value, DevSuffix); found {
				field, ok = fields[base]
			}
		}
		if !ok {
			return unknownKey(keyNode, fields, prefix)
		}
		child := prefix + keyNode.Value + "."
		if elem := structType(field.Type); elem != nil {
			if err := checkKeys(valueNode, elem, child); err != nil {
				return err
			}
			continue
		}
		if field.Type.Kind() == reflect.Map && structType(field.Type.Elem()) != nil && valueNode.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(valueNode.Content); j += 2 {
				if err := checkKeys(valueNode.Content[j+1], structType(field.Type.Elem()), child+valueNode.Content[j].Value+"."); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func unknownKey(keyNode *yaml.Node, fields map[string]reflect.StructField, prefix string) *Error {
	err := &Error{Line: keyNode.Line, Key: prefix + keyNode.Value, Message: "unknown key"}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	if _, isWebKey := schemaFields(reflect.TypeOf(Web{}))[keyNode.Value]; isWebKey && strings.HasPrefix(prefix, "processes.") {
		err.Hint = "web keys apply to the whole app; move it to the top level of the app file"
	} else if best := closest(keyNode.Value, names); best != "" {
		err.Hint = fmt.Sprintf("did you mean %q?", best)
	} else if len(names) <= 12 {
		err.Hint = "valid keys here: " + strings.Join(names, ", ")
	} else {
		err.Hint = "run `dboss config --reference` for the list of keys"
	}
	return err
}

// structType returns the struct a field decodes into, through pointers, or nil for leaves.
func structType(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return nil
	}
	// Types with their own decoder are leaves even when they are structs.
	if reflect.PointerTo(typ).Implements(reflect.TypeFor[yaml.Unmarshaler]()) {
		return nil
	}
	return typ
}

// schemaFields flattens inline embedded structs the way the decoder does, keyed by YAML name.
func schemaFields(typ reflect.Type) map[string]reflect.StructField {
	result := map[string]reflect.StructField{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Anonymous {
			for key, inner := range schemaFields(field.Type) {
				result[key] = inner
			}
			continue
		}
		if !field.IsExported() {
			continue
		}
		key, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if key == "" {
			key = strings.ToLower(field.Name)
		}
		if key != "-" {
			result[key] = field
		}
	}
	return result
}

// closest suggests a candidate within a small edit distance of value, or "" when nothing is near.
func closest(value string, candidates []string) string {
	best, bestDistance := "", 3
	for _, candidate := range candidates {
		if distance := editDistance(value, candidate); distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	return best
}

func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
