package proxy

import (
	"net/http"

	"dboss/internal/authcog"
	"dboss/internal/pages"
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
		TTL:           app.Web.SessionTTL.Value(),
		Allow:         app.Web.AuthAllows,
	}
}

// signIn is the AuthCog gate of an app with auth. The email list is checked on
// every request, so removing an entry ends that session with the next rescan.
func (h *Handler) signIn(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	if len(app.Web.Auth) == 0 {
		next()
		return
	}
	if h.exempt(r, app) {
		next()
		return
	}
	gate := signInGate(app, h.hostConfig().AuthCogRealm)
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
		pages.Page{Name: pages.SignedOut, App: app.Name, Action: `<a href="/">Sign in again</a>`}.Write(w, h.pageDirs(app)...)
		return
	}
	if session, ok := h.signin.Session(r, gate); ok && app.Web.AuthAllows(session.Email) {
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
