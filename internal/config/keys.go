package config

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Key is one documented configuration key: what it is, where it goes and what it holds.
// Path, Type, Types, Default and PerProcess come from the config structs and Default(), so they
// cannot drift; Block, Name, Description, Enum and the flags come from KeySpec in keyspecs.go.
type Key struct {
	Path        string   `json:"path"`
	Block       string   `json:"block"`
	Scope       Scope    `json:"scope"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Types       []string `json:"types"`
	Description string   `json:"description"`
	Default     string   `json:"default,omitempty"`
	Example     string   `json:"example,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Secret      bool     `json:"secret,omitempty"`
	PerProcess  bool     `json:"per_process,omitempty"`
}

// Keys lists every configuration key in display order: service keys, app keys, then the keys
// shared by both (shown once, with scope both).
func Keys() []Key {
	defaults := Default()
	var keys []Key
	walk(reflect.ValueOf(defaults), "", false, func(key Key) {
		if strings.HasPrefix(key.Path, "defaults.") {
			return
		}
		keys = append(keys, key)
	})
	app := App{Autostart: AutostartOn}
	walk(reflect.ValueOf(app), "", false, func(key Key) {
		if key.Scope == ScopeBoth {
			return
		}
		keys = append(keys, key)
	})
	walk(reflect.ValueOf(defaults.Defaults.Process), "", true, func(key Key) { keys = append(keys, key) })
	walk(reflect.ValueOf(defaults.Defaults.Web), "", false, func(key Key) { keys = append(keys, key) })
	walk(reflect.ValueOf(defaults.Defaults.Deploy), "", false, func(key Key) { keys = append(keys, key) })
	return keys
}

// KeyPaths returns every path the registry documents, for the coverage test.
func KeyPaths() []string {
	paths := make([]string, 0, len(keySpecs))
	for path := range keySpecs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// walk visits the yaml-tagged fields of value depth first, expanding inline structs in place
// and nested structs under their key. Leaf values become Keys.
func walk(value reflect.Value, prefix string, perProcess bool, visit func(Key)) {
	valueType := value.Type()
	for i := 0; i < valueType.NumField(); i++ {
		field := valueType.Field(i)
		tag := field.Tag.Get("yaml")
		if tag == "" || tag == "-" || !field.IsExported() {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		if strings.Contains(options, "inline") {
			walk(value.Field(i), prefix, perProcess, visit)
			continue
		}
		path := prefix + name
		if field.Type.Kind() == reflect.Struct {
			walk(value.Field(i), path+".", perProcess, visit)
			continue
		}
		spec := keySpecs[path]
		types := typesOf(field.Type)
		key := Key{
			Path:        path,
			Block:       spec.Block,
			Scope:       blockScope(spec.Block),
			Name:        spec.Name,
			Type:        strings.Join(types, " | "),
			Types:       types,
			Description: spec.Description,
			Enum:        spec.Enum,
			Required:    spec.Required,
			Secret:      spec.Secret,
			PerProcess:  perProcess,
		}
		key.Default = formatValue(value.Field(i))
		key.Example = spec.Example
		if key.Example == key.Default {
			key.Example = ""
		}
		visit(key)
	}
}

// typesOf lists the YAML shapes a field accepts. Most Go types have one; List also takes a
// single scalar, and Autostart the booleans plus the word button.
func typesOf(t reflect.Type) []string {
	switch t {
	case reflect.TypeOf(Duration(0)):
		return []string{"duration"}
	case reflect.TypeOf(Size(0)):
		return []string{"size"}
	case reflect.TypeOf(List(nil)):
		return []string{"string", "list"}
	case reflect.TypeOf(Autostart("")):
		return []string{"bool", "button"}
	}
	switch t.Kind() {
	case reflect.String:
		return []string{"string"}
	case reflect.Int:
		return []string{"int"}
	case reflect.Bool:
		return []string{"bool"}
	case reflect.Slice:
		return []string{"list"}
	case reflect.Array:
		return []string{"[from, to]"}
	case reflect.Map:
		return []string{"map"}
	}
	return []string{t.Kind().String()}
}

// formatValue renders a default the way it is written in dboss.yaml. Empty strings, lists
// and maps render as "" so the caller falls back to the example.
func formatValue(value reflect.Value) string {
	switch v := value.Interface().(type) {
	case Duration:
		return formatDuration(v.Value())
	case Size:
		return v.String()
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case bool:
		return strconv.FormatBool(v)
	case [2]int:
		return fmt.Sprintf("[%d, %d]", v[0], v[1])
	case List:
		return formatValue(reflect.ValueOf([]string(v)))
	case []string:
		if len(v) == 0 {
			return ""
		}
		return "[" + strings.Join(v, ", ") + "]"
	case []any:
		if len(v) == 0 {
			return ""
		}
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = fmt.Sprint(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]string:
		if len(v) == 0 {
			return ""
		}
		names := slices.Sorted(maps.Keys(v))
		parts := make([]string, len(names))
		for i, name := range names {
			parts[i] = name + ": " + v[name]
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	if value.Kind() == reflect.Map && value.Len() == 0 {
		return ""
	}
	return fmt.Sprint(value.Interface())
}

func formatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}
