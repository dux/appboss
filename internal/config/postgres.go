package config

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"

	"gopkg.in/yaml.v3"
)

// Postgres is the host's PostgreSQL server. The console inspects it read-only and the daemon
// backs up the selected databases on a schedule. An empty DSN auto-detects a local server;
// `postgres: false` turns the whole feature off.
type Postgres struct {
	Enabled bool            `yaml:"-" json:"enabled"`
	DSN     string          `yaml:"dsn" json:"dsn,omitempty"`
	Backups PostgresBackups `yaml:"backups" json:"backups"`
}

// PostgresBackups maps each database dumped by the daily run to its rotation window: week or
// month, empty meaning week. Manual backups from the console are kept outside the window.
type PostgresBackups map[string]string

// postgresFields is Postgres without its decoder, so the mapping form decodes through the tags.
type postgresFields Postgres

// UnmarshalYAML accepts false (off) or the {dsn, backups} mapping. The keys are checked here
// because a custom decoder is a leaf as far as the schema walk is concerned.
func (p *Postgres) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!bool" {
		p.Enabled = node.Value == "true"
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return &Error{Line: node.Line, Key: "postgres", Message: "must be false or a {dsn, backups} mapping"}
	}
	if err := checkKeys(node, reflect.TypeFor[postgresFields](), "postgres."); err != nil {
		return err
	}
	p.Enabled = true
	return node.Decode((*postgresFields)(p))
}

// Selected lists the databases configured for backup.
func (b PostgresBackups) Selected() []string {
	names := make([]string, 0, len(b))
	for name := range b {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Rotation returns the dump rotation window for one database: week, month, or empty when the
// database is not selected.
func (b PostgresBackups) Rotation(name string) string {
	rotation, ok := b[name]
	if ok && rotation == "" {
		return "week"
	}
	return rotation
}

var databaseName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}$`)

func validatePostgres(p Postgres) error {
	if !p.Enabled {
		return nil
	}
	for _, name := range p.Backups.Selected() {
		if !databaseName.MatchString(name) {
			return &Error{Key: "backups", Message: fmt.Sprintf("invalid database name %q", name), Hint: "names match [A-Za-z_][A-Za-z0-9_$]*, e.g. myapp_production"}
		}
		if rotation := p.Backups[name]; !slices.Contains([]string{"", "week", "month"}, rotation) {
			return keyErr("backups."+name, "must be week or month, not %q", rotation)
		}
	}
	return nil
}

// MarshalYAML writes false when the feature is off, so a resolved config reads back the same.
func (p Postgres) MarshalYAML() (any, error) {
	if !p.Enabled {
		return false, nil
	}
	return postgresFields(p), nil
}
