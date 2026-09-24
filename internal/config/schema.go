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
	{ID: "host", Title: "Host", Summary: "Where the apps and runtime files live, the port range and daily housekeeping.", Scope: ScopeService},
	{ID: "tokens", Title: "Tokens", Summary: "The GitHub token dboss pulls with and the token callers present to dboss.", Scope: ScopeService},
	{ID: "proxy", Title: "Proxy", Summary: "The public listener and how it routes and guards requests.", Scope: ScopeService},
	{ID: "management", Title: "Management console", Summary: "Hostnames and admins of the console.", Scope: ScopeService},
	{ID: "notify", Title: "Notifications", Summary: "One operator webhook for runtime events.", Scope: ScopeService},
	{ID: "postgres", Title: "PostgreSQL", Summary: "The server the console inspects and backs up.", Scope: ScopeService},
	{ID: "app", Title: "App", Summary: "Processes, hostnames, start policy and pages.", Scope: ScopeApp},
	{ID: "cron", Title: "Cron", Summary: "Scheduled one-shot commands.", Scope: ScopeApp},
	{ID: "hooks", Title: "Hooks", Summary: "One-shot commands triggered over HTTP.", Scope: ScopeApp},
	{ID: "lifecycle", Title: "Lifecycle", Summary: "Commands run when the app is created, started and destroyed.", Scope: ScopeApp},
	{ID: "runtime", Title: "Runtime", Summary: "How a process is checked, restarted, limited and logged.", Scope: ScopeBoth},
	{ID: "web", Title: "Web", Summary: "Proxy behaviour in front of the app.", Scope: ScopeBoth},
	{ID: "auth", Title: "Sign-in", Summary: "AuthCog sign-in in front of the app, or run for the app.", Scope: ScopeBoth},
	{ID: "alerts", Title: "Alerts", Summary: "Request log checks that post error-rate and slow events.", Scope: ScopeBoth},
	{ID: "events", Title: "Events", Summary: "Analytics events from log/*.json.log: retention, saved views and funnels.", Scope: ScopeBoth},
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
