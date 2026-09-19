package config

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// Template roles the init command can generate.
const (
	TemplateService = "service"
	TemplateApp     = "app"
)

// Template returns a fully commented YAML starter for a service (the root dboss.yaml) or an app
// (an app's dboss.yaml). Every setting is printed commented out with its default, or an example
// when it has none, so the operator uncomments only what they want and dboss fills the rest.
func Template(role string) (string, error) {
	lines, err := initLines(role)
	if err != nil {
		return "", err
	}
	title := "dboss service configuration (root dboss.yaml)"
	if role == TemplateApp {
		title = "dboss app configuration (an app's dboss.yaml)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n#\n", title)
	b.WriteString("# Every key is commented out with its default, or an example when it has none, in the\n")
	b.WriteString("# trailing note. Uncomment what you need, edit it, and delete the rest; dboss fills\n")
	b.WriteString("# any omitted key with the default shown. Validate with `dboss check -c dboss.yaml`.\n")
	for _, line := range lines {
		switch line.kind {
		case kindHeader:
			fmt.Fprintf(&b, "#\n# --- %s ---\n", line.text)
		default:
			fmt.Fprintf(&b, "# %s%s\n", strings.Repeat("  ", line.indent), line.text)
		}
	}
	b.WriteString("#\n# dboss init > dboss.yaml to save config\n")
	return b.String(), nil
}

const (
	kindYAML   = "yaml"
	kindHeader = "header"
)

// initLine is one output line: a commented YAML line, a section header, or a blank separator.
// key is the file-root dotted path for YAML leaf lines, empty otherwise.
type initLine struct {
	kind   string
	indent int
	text   string
	key    string
}

func initLines(role string) ([]initLine, error) {
	var value reflect.Value
	switch role {
	case TemplateService:
		value = reflect.ValueOf(Default())
	case TemplateApp:
		value = reflect.ValueOf(App{WebProcess: "web", Autostart: AutostartOn, Defaults: Default().Defaults})
	default:
		return nil, fmt.Errorf("unknown config type %q (use service or app)", role)
	}
	var lines []initLine
	lastSection := ""
	collectInit(&lines, &lastSection, value, "", 0)
	return lines, nil
}

// collectInit walks a config struct the way the decoder does and appends a commented line per
// leaf, plus a section header when the block changes at the top level.
func collectInit(lines *[]initLine, lastSection *string, value reflect.Value, prefix string, indent int) {
	valueType := value.Type()
	for i := 0; i < valueType.NumField(); i++ {
		field := valueType.Field(i)
		tag := field.Tag.Get("yaml")
		if tag == "" || tag == "-" || !field.IsExported() {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		if strings.Contains(options, "inline") {
			collectInit(lines, lastSection, value.Field(i), prefix, indent)
			continue
		}
		path := prefix + name
		fieldValue := value.Field(i)
		if structType(field.Type) != nil {
			appendSection(lines, lastSection, sectionTitle(path, fieldValue), indent)
			*lines = append(*lines, initLine{kind: kindYAML, indent: indent, text: name + ":"})
			collectInit(lines, lastSection, fieldValue, path+".", indent+1)
			continue
		}
		spec := keySpecs[normalizePath(path)]
		appendSection(lines, lastSection, blockTitle(spec.Block), indent)
		*lines = append(*lines, initLine{kind: kindYAML, indent: indent, key: path, text: renderInitLeaf(name, fieldValue, spec)})
	}
}

func appendSection(lines *[]initLine, lastSection *string, title string, indent int) {
	if indent != 0 || title == "" || title == *lastSection {
		return
	}
	*lines = append(*lines, initLine{kind: kindHeader, text: title})
	*lastSection = title
}

// sectionTitle names a top-level struct: the defaults wrapper keeps its own name, everything
// else takes the title of the block its first key belongs to.
func sectionTitle(path string, value reflect.Value) string {
	if path == "defaults" {
		return "Per-app defaults"
	}
	if block := firstBlock(path+".", value); block != "" {
		return blockTitle(block)
	}
	return path
}

// firstBlock returns the block of the first leaf under value, so a struct can be labelled.
func firstBlock(prefix string, value reflect.Value) string {
	valueType := value.Type()
	for i := 0; i < valueType.NumField(); i++ {
		field := valueType.Field(i)
		tag := field.Tag.Get("yaml")
		if tag == "" || tag == "-" || !field.IsExported() {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		if structType(field.Type) != nil {
			childPrefix := prefix
			if !strings.Contains(options, "inline") {
				childPrefix = prefix + name + "."
			}
			if block := firstBlock(childPrefix, value.Field(i)); block != "" {
				return block
			}
			continue
		}
		if spec, ok := keySpecs[prefix+name]; ok {
			return spec.Block
		}
	}
	return ""
}

func blockTitle(id string) string {
	for _, block := range blocks {
		if block.ID == id {
			return block.Title
		}
	}
	return id
}

// normalizePath maps a rendered file path back to the registry path: the shared app keys live
// under defaults: in the root file but keep their bare name in the registry.
func normalizePath(path string) string {
	return strings.TrimPrefix(path, "defaults.")
}

// renderInitLeaf renders one commented YAML line with its value and a note saying whether the
// value is the default, an example, or both. An example equal to the default is dropped.
func renderInitLeaf(name string, value reflect.Value, spec KeySpec) string {
	def := initValue(value)
	example := spec.Example
	if example == def {
		example = ""
	}
	shown := def
	if shown == "" {
		shown = example
	}
	line := name + ":"
	if shown != "" {
		line = name + ": " + shown
	}
	var notes []string
	if def != "" {
		notes = append(notes, "default")
	}
	if example != "" {
		if def != "" {
			notes = append(notes, "e.g. "+example)
		} else {
			notes = append(notes, "example")
		}
	}
	if len(spec.Enum) > 0 {
		notes = append(notes, "one of: "+strings.Join(spec.Enum, ", "))
	}
	if spec.Required {
		notes = append(notes, "required")
	}
	if spec.Secret {
		notes = append(notes, "secret")
	}
	if len(notes) > 0 {
		line += "  # " + strings.Join(notes, "; ")
	}
	return line
}

// initValue renders a default the way it should be written in the template. Lists are quoted
// element by element, so a single value such as ":80" stays valid YAML.
func initValue(value reflect.Value) string {
	switch v := value.Interface().(type) {
	case Duration:
		return formatDuration(v.Value())
	case Size:
		return v.String()
	case string:
		return quoteScalar(v)
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case [2]int:
		return fmt.Sprintf("[%d, %d]", v[0], v[1])
	case List:
		return quoteList([]string(v))
	case []string:
		return quoteList(v)
	case []any:
		return formatValue(value)
	}
	return formatValue(value)
}

func quoteList(items []string) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = quoteScalar(item)
	}
	return bracket(parts)
}

func bracket(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// quoteScalar quotes a string when the plain form would not parse back as the same string.
// Composite examples such as [..] or {..} are authored by hand and never pass through here.
func quoteScalar(value string) string {
	if value == "" {
		return ""
	}
	if isPlainSafe(value) {
		return value
	}
	return strconv.Quote(value)
}

func isPlainSafe(value string) bool {
	if strings.ContainsAny(value, ":#[]{},&*!|>'\"%@`") {
		return false
	}
	if strings.ContainsRune("-?:,[]{}#&*!|>'\"%@`", rune(value[0])) {
		return false
	}
	return !reservedScalar.MatchString(value) && !isNumber(value)
}

var reservedScalar = regexp.MustCompile(`(?i)^(true|false|yes|no|on|off|null|~)$`)

func isNumber(value string) bool {
	_, err := strconv.ParseFloat(value, 64)
	return err == nil
}
