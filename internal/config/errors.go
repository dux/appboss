package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
