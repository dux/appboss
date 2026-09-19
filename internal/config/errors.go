package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Error is one problem in one config file, with enough position to point an editor at it.
type Error struct {
	Path    string // file the problem is in, empty until Parse attaches it
	Line    int    // 1-based, 0 when unknown
	Key     string // dotted key path such as defaults.idle_stop, empty for file-level problems
	Message string
	Hint    string // one line of help printed under the message
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Path != "" {
		b.WriteString(displayPath(e.Path))
		if e.Line > 0 {
			b.WriteString(":" + strconv.Itoa(e.Line))
		}
		b.WriteString(": ")
	}
	if e.Key != "" {
		b.WriteString(e.Key + ": ")
	}
	b.WriteString(e.Message)
	if e.Hint != "" {
		b.WriteString("\n  " + e.Hint)
	}
	return b.String()
}

func keyErr(key, format string, args ...any) *Error {
	return &Error{Key: key, Message: fmt.Sprintf(format, args...)}
}

// scoped prefixes the key of a config error with the section it was found in.
func scoped(err error, prefix string) error {
	var cfgErr *Error
	if !errors.As(err, &cfgErr) {
		return keyErr(prefix, "%v", err)
	}
	if cfgErr.Key == "" {
		cfgErr.Key = prefix
	} else {
		cfgErr.Key = prefix + "." + cfgErr.Key
	}
	return cfgErr
}

// displayPath keeps paths short and clickable: relative to the working directory when inside it.
func displayPath(path string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return path
	}
	relative, err := filepath.Rel(cwd, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return "./" + relative
}

var yamlSyntaxPattern = regexp.MustCompile(`^yaml: (?:line (\d+): )?(.*)$`)

// located turns whatever the decoder returned into an Error with the file attached and, when the
// error carries a line but no key, the key found at that line.
func located(err error, path string, root *yaml.Node) *Error {
	var cfgErr *Error
	if !errors.As(err, &cfgErr) {
		cfgErr = &Error{Message: err.Error()}
		if match := yamlSyntaxPattern.FindStringSubmatch(err.Error()); match != nil {
			cfgErr.Line, _ = strconv.Atoi(match[1])
			cfgErr.Message = "syntax error: " + strings.TrimSpace(match[2])
		}
	}
	cfgErr.Path = path
	if cfgErr.Line == 0 && cfgErr.Key != "" && root != nil {
		cfgErr.Line = lineOf(root, cfgErr.Key)
	}
	if cfgErr.Key == "" && cfgErr.Line > 0 && root != nil {
		cfgErr.Key = keyAt(root, cfgErr.Line, "")
	}
	return cfgErr
}

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

// lineOf finds the line of a dotted key path in the document, 0 when absent.
func lineOf(root *yaml.Node, key string) int {
	node := root
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	for _, part := range strings.Split(key, ".") {
		if node.Kind != yaml.MappingNode {
			return 0
		}
		found := false
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == part {
				if node.Content[i+1].Kind != yaml.MappingNode {
					return node.Content[i].Line
				}
				node = node.Content[i+1]
				found = true
				break
			}
		}
		if !found {
			return 0
		}
	}
	return node.Line
}

// keyAt returns the dotted path of the scalar value found on line, "" when there is none.
func keyAt(node *yaml.Node, line int, prefix string) string {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valueNode := node.Content[i], node.Content[i+1]
		if valueNode.Kind == yaml.MappingNode {
			if found := keyAt(valueNode, line, prefix+keyNode.Value+"."); found != "" {
				return found
			}
			continue
		}
		if valueNode.Line == line || keyNode.Line == line {
			return prefix + keyNode.Value
		}
	}
	return ""
}
