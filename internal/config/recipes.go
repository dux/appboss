package config

import "slices"

// Recipe is one curated group of related configuration keys, the data behind the console's
// visual config form. Scope says which file a recipe belongs to: a host recipe edits the root
// dboss.yaml (or its server override), an app recipe edits one app's dboss.yaml.
type Recipe struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Scope       string        `json:"scope"`
	Fields      []RecipeField `json:"fields"`
}

// Recipe scopes. The root file carries the proxy, console and storage keys; an app file carries
// the per-app process and web keys.
const (
	RecipeHost = "host"
	RecipeApp  = "app"
)

// RecipeField is one form field. Path, Type, Default, Example, Description and Options come from
// Keys, so the form and the key reference can never disagree; Label is the key name and Section
// is chosen here because it is presentation, not config schema.
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
	section string
}

type recipeSpec struct {
	id          string
	title       string
	description string
	scope       string
	fields      []recipeField
}

// recipeSpecs is the curated form: which keys appear together and under which section. Every
// field names a documented key, so a new key is offered in the form by naming it here.
var recipeSpecs = []recipeSpec{
	{
		id:          "auth",
		title:       "Sign-in",
		description: "Ask visitors to sign in through AuthCog and let only the listed emails reach the app.",
		scope:       RecipeApp,
		fields: []recipeField{
			{path: "auth.allow_emails"},
			{path: "auth.session_ttl"},
		},
	},
	{
		id:          "authcog",
		title:       "AuthCog login",
		description: "Run the AuthCog sign-in for the app and hand it the profile once; the app creates its own session.",
		scope:       RecipeApp,
		fields: []recipeField{
			{path: "authcog.login"},
			{path: "authcog.path"},
			{path: "authcog.realm"},
		},
	},
	{
		id:          "alerts",
		title:       "Alerts",
		description: "Post to the notify webhook when the app answers with too many errors or too slowly.",
		scope:       RecipeApp,
		fields: []recipeField{
			{path: "alerts.error_rate"},
			{path: "alerts.slow_p95"},
			{path: "alerts.window"},
			{path: "alerts.min_requests"},
		},
	},
	{
		id:          "web",
		title:       "Web",
		description: "Static policy and the proxy behaviour in front of the app. Hosts, the static directory and the canonical host are declared on the web process in the YAML editor.",
		scope:       RecipeApp,
		fields: []recipeField{
			{path: "autostart"},
			{path: "deletable"},
			{path: "static_immutable"},
			{path: "static_extensions"},
			{path: "health_endpoint"},
			{path: "max_body"},
			{path: "allow_ips"},
			{path: "headers"},
			{path: "basic_auth"},
			{path: "maintenance_page"},
			{path: "error_page_path"},
		},
	},
	{
		id:          "runtime",
		title:       "Health and runtime",
		description: "How the app is checked, restarted, limited and logged.",
		scope:       RecipeApp,
		fields: []recipeField{
			{path: "idle_stop", section: "Health"},
			{path: "health_interval", section: "Health"},
			{path: "health_timeout", section: "Health"},
			{path: "liveness_interval", section: "Health"},
			{path: "unhealthy_threshold", section: "Health"},
			{path: "restart", section: "Restart"},
			{path: "max_restarts", section: "Restart"},
			{path: "restart_reset", section: "Restart"},
			{path: "restart_backoff", section: "Restart"},
			{path: "stop_timeout", section: "Restart"},
			{path: "stop_signal", section: "Restart"},
			{path: "resources", section: "Resources"},
			{path: "memory_max", section: "Resources"},
			{path: "cpu_max", section: "Resources"},
			{path: "shell", section: "Resources"},
			{path: "env", section: "Resources"},
			{path: "log_max_size", section: "Logs"},
			{path: "log_keep", section: "Logs"},
			{path: "log_tail_lines", section: "Logs"},
			{path: "log_retention", section: "Logs"},
			{path: "stdout_retention", section: "Logs"},
			{path: "log_flush", section: "Logs"},
		},
	},
	{
		id:          "notifications",
		title:       "Notifications",
		description: "Post runtime events to one operator webhook.",
		scope:       RecipeHost,
		fields: []recipeField{
			{path: "notify.url"},
			{path: "notify.format"},
			{path: "notify.events"},
			{path: "notify.min_interval"},
			{path: "notify.headers"},
		},
	},
	{
		id:          "postgres",
		title:       "PostgreSQL connection",
		description: "How dboss reaches the server it inspects and backs up.",
		scope:       RecipeHost,
		fields: []recipeField{
			{path: "postgres.enabled"},
			{path: "postgres.dsn"},
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
				Label:       key.Name,
				Section:     field.section,
				Kind:        widgetKind(key),
				Type:        key.Type,
				Default:     key.Default,
				Example:     key.Example,
				Description: key.Description,
				Options:     key.Enum,
				PerProcess:  key.PerProcess,
			})
		}
		recipes = append(recipes, recipe)
	}
	return recipes
}

// widgetKind is the form control a field needs. An enum always means a select; a list, even as
// one shape of a union, means the multi-line textarea.
func widgetKind(key Key) string {
	if len(key.Enum) > 0 {
		return "select"
	}
	if slices.Contains(key.Types, "list") {
		return "list"
	}
	for _, typ := range key.Types {
		switch typ {
		case "int":
			return "number"
		case "bool":
			return "bool"
		case "duration":
			return "duration"
		case "size":
			return "size"
		case "map":
			return "map"
		case "[from, to]":
			return "range"
		}
	}
	return "text"
}
