package config

// Recipe is one curated group of related configuration keys, the data behind the console's
// visual config form. Scope says which file a recipe belongs to: a host recipe edits the host
// appboss.yaml (or its server override), an app recipe edits one app's appboss.yaml.
type Recipe struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Scope       string        `json:"scope"`
	Fields      []RecipeField `json:"fields"`
}

// Recipe scopes. A host file carries the proxy, console and storage keys; an app file carries
// the per-app process and web keys.
const (
	RecipeHost = "host"
	RecipeApp  = "app"
)

// RecipeField is one form field. Path, Type, Default, Example and Description come from Keys,
// so the form and the key reference can never disagree; Label, Section and Options are chosen
// here because they are presentation, not config schema.
type RecipeField struct {
	Path        string   `json:"path"`
	Label       string   `json:"label"`
	Section     string   `json:"section,omitempty"`
	Kind        string   `json:"kind"`
	Type        string   `json:"type"`
	Default     string   `json:"default,omitempty"`
	Example     string   `json:"example,omitempty"`
	Description string   `json:"description"`
	Options     []string `json:"options,omitempty"`
	PerProcess  bool     `json:"per_process,omitempty"`
}

type recipeField struct {
	path    string
	label   string
	section string
	options []string
}

type recipeSpec struct {
	id          string
	title       string
	description string
	scope       string
	fields      []recipeField
}

// recipeSpecs is the curated form. A field is a path documented in keyDocs, so a new key is
// added to a recipe by naming it here and, when it is an enum, listing its values.
var recipeSpecs = []recipeSpec{
	{
		id:          "pubsub",
		title:       "Realtime channels",
		description: "Serve WebSocket channels on the app's hosts and choose who may publish.",
		scope:       RecipeApp,
		fields: []recipeField{
			{path: "pubsub.path", label: "Path prefix"},
			{path: "pubsub.secret", label: "Publish secret"},
			{path: "pubsub.replay", label: "Replay buffer"},
			{path: "pubsub.max_clients", label: "Max subscribers"},
			{path: "pubsub.max_message_size", label: "Max message size"},
			{path: "pubsub.client_events", label: "Client publishing"},
			{path: "pubsub.test", label: "Self-test page"},
		},
	},
	{
		id:          "web",
		title:       "Web",
		description: "Hostnames, static files and the proxy behaviour in front of the app.",
		scope:       RecipeApp,
		fields: []recipeField{
			{path: "hosts", label: "Hostnames"},
			{path: "canonical_host", label: "Canonical host"},
			{path: "web_process", label: "Web process"},
			{path: "autostart", label: "Start policy", options: []string{"true", "false", "button"}},
			{path: "static", label: "Static directory"},
			{path: "static_immutable", label: "Immutable prefixes"},
			{path: "health_endpoint", label: "Health endpoint"},
			{path: "max_body", label: "Max body size"},
			{path: "allow_ips", label: "Allowed IPs"},
			{path: "headers", label: "Response headers"},
			{path: "basic_auth", label: "Basic auth users"},
			{path: "maintenance_page", label: "Maintenance page"},
		},
	},
	{
		id:          "runtime",
		title:       "Health and runtime",
		description: "How the app is checked, restarted, limited and logged.",
		scope:       RecipeApp,
		fields: []recipeField{
			{path: "idle_stop", label: "Idle stop", section: "Health"},
			{path: "health", label: "Readiness check", section: "Health"},
			{path: "health_interval", label: "Check interval", section: "Health"},
			{path: "health_timeout", label: "Readiness timeout", section: "Health"},
			{path: "unhealthy_threshold", label: "Liveness failures", section: "Health"},
			{path: "restart", label: "Restart policy", section: "Restart", options: []string{"on-failure", "always", "never"}},
			{path: "max_restarts", label: "Max restarts", section: "Restart"},
			{path: "restart_reset", label: "Failure reset", section: "Restart"},
			{path: "restart_backoff", label: "Backoff", section: "Restart"},
			{path: "stop_timeout", label: "Stop timeout", section: "Restart"},
			{path: "stop_signal", label: "Stop signal", section: "Restart", options: []string{"TERM", "INT", "QUIT", "USR1", "USR2"}},
			{path: "resources", label: "Resource backend", section: "Resources", options: []string{"auto", "cgroup", "procgroup"}},
			{path: "memory_max", label: "Memory limit", section: "Resources"},
			{path: "cpu_max", label: "CPU limit", section: "Resources"},
			{path: "shell", label: "Run through shell", section: "Resources"},
			{path: "env", label: "Environment", section: "Resources"},
			{path: "log_max_size", label: "Max log size", section: "Logs"},
			{path: "log_keep", label: "Kept log files", section: "Logs"},
			{path: "log_tail_lines", label: "Tail lines", section: "Logs"},
			{path: "log_retention", label: "Log retention", section: "Logs"},
			{path: "stdout_retention", label: "Stdout retention", section: "Logs"},
			{path: "log_flush", label: "Log flush", section: "Logs"},
		},
	},
	{
		id:          "s3",
		title:       "S3 object storage",
		description: "Off-host backup copies. Empty endpoint and bucket disable object storage.",
		scope:       RecipeHost,
		fields: []recipeField{
			{path: "s3.endpoint", label: "Endpoint", section: "Connection"},
			{path: "s3.region", label: "Region", section: "Connection"},
			{path: "s3.bucket", label: "Bucket", section: "Connection"},
			{path: "s3.prefix", label: "Key prefix", section: "Connection"},
			{path: "s3.access_key", label: "Access key ID", section: "Credentials"},
			{path: "s3.secret_key", label: "Secret access key", section: "Credentials"},
			{path: "s3.path_style", label: "Path-style addressing", section: "Options"},
			{path: "s3.sse", label: "Server-side encryption", section: "Options"},
		},
	},
	{
		id:          "notifications",
		title:       "Notifications",
		description: "Post runtime events to one operator webhook.",
		scope:       RecipeHost,
		fields: []recipeField{
			{path: "notify.url", label: "Webhook URL"},
			{path: "notify.format", label: "Format", options: []string{"generic", "slack", "discord", "ntfy"}},
			{path: "notify.events", label: "Events"},
			{path: "notify.min_interval", label: "Quiet period"},
			{path: "notify.headers", label: "Extra headers"},
		},
	},
	{
		id:          "postgres",
		title:       "PostgreSQL connection",
		description: "How appboss reaches the server it inspects and backs up.",
		scope:       RecipeHost,
		fields: []recipeField{
			{path: "postgres.enabled", label: "Enable PostgreSQL"},
			{path: "postgres.dsn", label: "Connection string"},
		},
	},
}

// Recipes lists every recipe with its field metadata resolved from Keys. Fields keep the order
// declared in recipeSpecs; the caller filters by scope.
func Recipes() []Recipe {
	keys := map[string]Key{}
	for _, key := range Keys() {
		keys[key.Path] = key
	}
	recipes := make([]Recipe, 0, len(recipeSpecs))
	for _, spec := range recipeSpecs {
		recipe := Recipe{ID: spec.id, Title: spec.title, Description: spec.description, Scope: spec.scope}
		for _, field := range spec.fields {
			key := keys[field.path]
			recipe.Fields = append(recipe.Fields, RecipeField{
				Path:        key.Path,
				Label:       field.label,
				Section:     field.section,
				Kind:        widgetKind(key, field.options),
				Type:        key.Type,
				Default:     key.Default,
				Example:     key.Example,
				Description: key.Description,
				Options:     field.options,
				PerProcess:  key.PerProcess,
			})
		}
		recipes = append(recipes, recipe)
	}
	return recipes
}

// widgetKind is the form control a field needs. An explicit option list always means a select.
func widgetKind(key Key, options []string) string {
	if len(options) > 0 {
		return "select"
	}
	switch key.Type {
	case "int":
		return "number"
	case "bool":
		return "bool"
	case "duration":
		return "duration"
	case "size":
		return "size"
	case "list":
		return "list"
	case "map":
		return "map"
	case "[from, to]":
		return "range"
	}
	return "text"
}
