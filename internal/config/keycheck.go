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
		if valueNode.Kind == yaml.MappingNode && field.Type.Kind() != reflect.Map && field.Type.Kind() != reflect.Struct && len(valueNode.Content) > 0 {
			// A key that used to be a block and is now a value, such as auth or ports.
			if hint := movedHint(child + valueNode.Content[0].Value); hint != "" {
				return &Error{Line: valueNode.Content[0].Line, Key: child + valueNode.Content[0].Value, Message: "was removed", Hint: hint}
			}
		}
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

// movedKeys names the replacement of every key the config dropped, keyed by its path with the
// defaults. and processes.<name>. prefixes removed, so an old file fails with the way forward.
var movedKeys = map[string]string{
	"state_dir":                              "replaced by dir (state/ lives inside it)",
	"log_dir":                                "replaced by dir (log/ lives inside it)",
	"socket":                                 "replaced by dir (dboss.sock lives inside it)",
	"ports.range":                            "write the range directly: ports: [3100, 3990]",
	"daemon":                                 "log_level and audit_retention moved to the top level; prune_at and vacuum_at are maintenance_at; the rest are built in",
	"proxy.trusted_cidrs":                    "use proxy.cloudflare: true, or allow_ips per app",
	"proxy.client_ip_headers":                "use proxy.cloudflare: true",
	"proxy.cloudflare_only":                  "use proxy.cloudflare: true",
	"proxy.wake":                             "the pages are files in the pages folder; see dboss pages",
	"proxy.upstream":                         "response_header_timeout is proxy.timeout; the rest are built in",
	"proxy.tls.directory":                    "certificates always come from Let's Encrypt",
	"proxy.tls.cache_dir":                    "certificates are cached under dir",
	"proxy.tls.redirect":                     "plain http always redirects when proxy.tls.listen is set",
	"management.url":                         "the console is https://<first management.host>",
	"management.auth":                        "admin_emails is management.admins, realm is authcog_realm, session_ttl is defaults.session_ttl",
	"management.metrics":                     "/metrics is gated by tokens.dboss",
	"notify.format":                          "the payload follows the webhook URL",
	"notify.min_interval":                    "built in at 5m",
	"postgres.enabled":                       "write postgres: false to turn it off",
	"postgres.backup":                        "write postgres.backups: {database: week}",
	"github_token":                           "moved to tokens.github in the host dboss.yaml",
	"health_interval":                        "built in",
	"restart_reset":                          "built in",
	"restart_backoff":                        "built in",
	"log_max_size":                           "built in",
	"log_keep":                               "built in",
	"log_tail_lines":                         "built in",
	"resources":                              "cgroup v2 is used whenever it is writable",
	"maintenance_page":                       "put maintenance.html in the pages folder",
	"error_page_path":                        "put error.html in the pages folder",
	"auth.allow_emails":                      "write the list directly: auth: [ana@example.com]",
	"auth.session_ttl":                       "moved to session_ttl",
	"authcog.login":                          "write authcog: true or authcog: /path",
	"authcog.path":                           "write authcog: /path",
	"authcog.realm":                          "moved to authcog_realm in the host dboss.yaml",
	"alerts.window":                          "built in at 5m",
	"alerts.min_requests":                    "built in at 20",
	"management.metrics.token":               "moved to tokens.dboss",
	"management.auth.admin_emails":           "moved to management.admins",
	"management.auth.realm":                  "moved to authcog_realm",
	"management.auth.session_ttl":            "moved to defaults.session_ttl",
	"proxy.upstream.response_header_timeout": "moved to proxy.timeout",
}

// movedHint returns the replacement note for a dropped key, looked up by its own path and then
// by each parent.
func movedHint(path string) string {
	path = strings.TrimPrefix(path, "defaults.")
	if rest, ok := strings.CutPrefix(path, "processes."); ok {
		if _, key, found := strings.Cut(rest, "."); found {
			path = key
		}
	}
	for {
		if hint, ok := movedKeys[path]; ok {
			return hint
		}
		cut := strings.LastIndex(path, ".")
		if cut < 0 {
			return ""
		}
		path = path[:cut]
	}
}

func unknownKey(keyNode *yaml.Node, fields map[string]reflect.StructField, prefix string) *Error {
	err := &Error{Line: keyNode.Line, Key: prefix + keyNode.Value, Message: "unknown key"}
	if hint := movedHint(prefix + keyNode.Value); hint != "" {
		err.Message, err.Hint = "was removed", hint
		return err
	}
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
