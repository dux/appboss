package proxy

import (
	"io"
	"net/http"

	"dboss/internal/authcog"
	"dboss/internal/supervisor"
)

const (
	signInCallbackPath = "/.well-known/dboss/auth"
	signInLogoutPath   = "/.well-known/dboss/logout"
	signInStateCookie  = "dboss_auth_state"
	signInCookie       = "dboss_auth"
	// userHeader carries the signed-in email to the app. It is removed from every inbound
	// request, so an app can trust it.
	userHeader = "X-Dboss-User"
)

const signedOutPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Signed out</title><style>body{background:#f1f5f9;color:#182433;font:15px/1.5 Inter,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;display:grid;min-height:100vh;place-items:center;margin:0}main{max-width:32rem;padding:0 1.5rem;text-align:center}a{color:#0054a6}p{color:#667382}</style><main><h1>Signed out</h1><p><a href="/">Sign in again</a></p></main></html>`

// signInGate describes one app to the shared AuthCog flow. ResolveHost already matched the
// request host to this app, so every host that gets here is one of its own.
func signInGate(app supervisor.Snapshot, realm string) authcog.Gate {
	return authcog.Gate{
		Audience:      "app:" + app.Name,
		Realm:         realm,
		Hosts:         func(string) bool { return true },
		CallbackPath:  signInCallbackPath,
		StateCookie:   signInStateCookie,
		SessionCookie: signInCookie,
		TTL:           app.Web.Auth.SessionTTL.Value(),
		Allow:         app.Web.Auth.Allows,
	}
}

// signIn is the AuthCog gate of an app with auth.allow_emails. The email list is checked on
// every request, so removing an entry ends that session with the next rescan.
func (h *Handler) signIn(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	if !app.Web.Auth.Enabled() {
		next()
		return
	}
	if h.exempt(r, app) {
		next()
		return
	}
	gate := signInGate(app, h.cfg.Management.Auth.Realm)
	switch r.URL.Path {
	case signInCallbackPath:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.signin.Callback(w, r, gate)
		return
	case signInLogoutPath:
		h.signin.ClearSession(w, r, gate)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, signedOutPage)
		return
	}
	if session, ok := h.signin.Session(r, gate); ok && app.Web.Auth.Allows(session.Email) {
		r.Header.Set(userHeader, session.Email)
		next()
		return
	}
	if !wantsHTML(r) {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	h.signin.Start(w, r, gate)
}
