package proxy

import (
	"encoding/json"
	"net/http"

	"dboss/internal/authcog"
	"dboss/internal/supervisor"
)

const (
	authCogStateCookie = "dboss_authcog_state"
	authCogAudience    = "authcog:"
)

// authCog is the app-level AuthCog login service. dboss runs the whole round trip at the app's
// configured path and then forwards one request to that same path with the profile in
// X-Dboss-User; the app reads it and creates its own session. dboss keeps nothing.
func (h *Handler) authCog(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	cfg := app.Web.AuthCog
	if !cfg.Enabled() || r.URL.Path != cfg.Path {
		next()
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	gate := authCogGate(app)
	if r.URL.Query().Get("callback") == "" {
		h.signin.Start(w, r, gate)
		return
	}
	profile, ok := h.signin.Authenticate(w, r, gate)
	if !ok {
		return
	}
	data, err := json.Marshal(profile)
	if err != nil {
		http.Error(w, "authentication failed", http.StatusInternalServerError)
		return
	}
	// The app sees the profile and the bare path: the one-time hash and challenge are spent.
	r.URL.RawQuery = ""
	r.Header.Set(userHeader, string(data))
	next()
}

// authCogGate describes the app to the shared AuthCog flow. Every host that reaches here already
// belongs to the app, so the gate answers for all of them. Any AuthCog account is admitted.
func authCogGate(app supervisor.Snapshot) authcog.Gate {
	return authcog.Gate{
		Audience:     authCogAudience + app.Name,
		Realm:        app.Web.AuthCog.RealmHost(),
		Hosts:        func(string) bool { return true },
		CallbackPath: app.Web.AuthCog.Path,
		StateCookie:  authCogStateCookie,
		Allow:        func(string) bool { return true },
	}
}

// withinPath reports whether path is prefix itself or one of its children.
func withinPath(prefix, path string) bool {
	return path == prefix || (prefix != "" && len(path) > len(prefix) && path[:len(prefix)+1] == prefix+"/")
}
