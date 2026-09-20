package config

import "slices"

// Scope says which config file a block may appear in. A service block lives in the root
// dboss.yaml, an app block in an app's dboss.yaml, and a both block appears in either file
// (under defaults: in the root file, at the top level of an app file).
type Scope string

const (
	ScopeService Scope = "service"
	ScopeApp     Scope = "app"
	ScopeBoth    Scope = "both"
)

// Block is one group of related configuration keys: the unit the key listing, the console keys
// view and the CLI group by. The doc for each key is the KeySpec entry in keyspecs.go.
type Block struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Scope   Scope  `json:"scope"`
}

// blocks is every block in display order: service, app, then the blocks shared by both.
var blocks = []Block{
	{ID: "paths", Title: "Paths", Summary: "Where the apps, runtime state and logs live.", Scope: ScopeService},
	{ID: "proxy", Title: "Proxy", Summary: "The public listener and how it routes and guards requests.", Scope: ScopeService},
	{ID: "management", Title: "Management console", Summary: "Hostnames, sign-in and the health and metrics endpoints.", Scope: ScopeService},
	{ID: "ports", Title: "Ports", Summary: "The port range dboss owns and allocates from.", Scope: ScopeService},
	{ID: "daemon", Title: "Daemon", Summary: "Background cadence, log level and audit retention.", Scope: ScopeService},
	{ID: "notify", Title: "Notifications", Summary: "One operator webhook for runtime events.", Scope: ScopeService},
	{ID: "postgres", Title: "PostgreSQL", Summary: "The server the console inspects and backs up.", Scope: ScopeService},
	{ID: "app", Title: "App", Summary: "Processes, hostnames and start policy.", Scope: ScopeApp},
	{ID: "cron", Title: "Cron", Summary: "Scheduled one-shot commands.", Scope: ScopeApp},
	{ID: "hooks", Title: "Hooks", Summary: "Signed one-shot commands triggered over HTTP.", Scope: ScopeApp},
	{ID: "runtime", Title: "Runtime", Summary: "How a process is checked, restarted, limited and logged.", Scope: ScopeBoth},
	{ID: "web", Title: "Web", Summary: "Proxy behaviour in front of the app.", Scope: ScopeBoth},
	{ID: "auth", Title: "Sign-in", Summary: "AuthCog sign-in in front of the app, for the listed emails only.", Scope: ScopeBoth},
	{ID: "authcog", Title: "AuthCog login", Summary: "AuthCog sign-in dboss runs for the app, handing it the profile once.", Scope: ScopeBoth},
	{ID: "alerts", Title: "Alerts", Summary: "Request log checks that post error-rate and slow events.", Scope: ScopeBoth},
}

func blockScope(id string) Scope {
	for _, block := range blocks {
		if block.ID == id {
			return block.Scope
		}
	}
	return ""
}

// Blocks returns every block in display order.
func Blocks() []Block { return slices.Clone(blocks) }
