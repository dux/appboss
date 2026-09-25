package proxy

import (
	"fmt"
	"net/http"

	"dboss/internal/pages"
	"dboss/internal/supervisor"
)

// Filter is one stage of the request pipeline. It either answers the request itself or calls
// next to continue. Stages run in the order they were added, so a module filter can slot in
// without disturbing the built-in ones.
type Filter func(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func())

// serve runs the pipeline for an app ResolveHost already picked.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot) {
	// Only dboss may hand an app identity, so a client-supplied header never survives.
	r.Header.Del(userHeader)
	var step func(int)
	step = func(index int) {
		if index >= len(h.filters) {
			return
		}
		h.filters[index](w, r, app, func() { step(index + 1) })
	}
	step(0)
}

// initFilters assembles the pipeline: built-ins, then extra module filters, then the forward
// stage that ends every request.
func (h *Handler) initFilters(extra ...Filter) {
	h.filters = append(h.filters[:0], h.canonical, h.allow, h.block, h.publicHealth, h.authCog, h.signIn, h.authorize, h.maintain, h.staticFiles, h.bufferBody)
	h.filters = append(h.filters, extra...)
	h.filters = append(h.filters, func(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, _ func()) {
		h.forward(w, r, app)
	})
}

// The built-in stages, in request order.

func (h *Handler) canonical(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	if web, ok := app.WebForHost(r.Host); ok && redirectCanonical(w, r, web.CanonicalHost) {
		return
	}
	next()
}

func (h *Handler) allow(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	if !allowed(clientIP(r, h.cfg.Proxy.Cloudflare), app.Web.AllowPrefixes()) {
		h.forbidden(w, r, app)
		return
	}
	next()
}

// block answers 403 for a path the app's deny list covers, before auth or the app is contacted.
func (h *Handler) block(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	if denied(r.URL.Path, app.Web.Deny) {
		if h.recorder != nil {
			_ = h.recorder.RecordBlocked(r.URL.Path)
		}
		h.blocked(w, r, app)
		return
	}
	next()
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	if authorized(r, app) || h.exempt(r, app) {
		next()
		return
	}
	w.Header().Set("WWW-Authenticate", fmt.Sprintf("Basic realm=%q", app.Name))
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

// exempt reports whether a request passes basic_auth and the sign-in gate on its own terms: the
// app owns its authcog login namespace, where dboss hands the identity over, and a module can
// vouch for a request, e.g. a pubsub publisher presenting its own secret.
func (h *Handler) exempt(r *http.Request, app supervisor.Snapshot) bool {
	if app.Web.AuthCog.Enabled() && withinPath(string(app.Web.AuthCog), r.URL.Path) {
		return true
	}
	return h.pubsub != nil && h.pubsub.AuthorizesPublish(r, app)
}

// publicHealth answers the app's own status path without auth, so a Cloudflare health check or
// uptime monitor can probe the app domain. It reports whether a visitor would be served: 200 while
// the app runs or sleeps until the next request, 503 otherwise, and it never wakes a stopped app.
func (h *Handler) publicHealth(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	path := app.Web.HealthEndpoint
	if path == "" || r.URL.Path != path || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		next()
		return
	}
	status := http.StatusOK
	if !app.Serving() {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method == http.MethodGet {
		fmt.Fprintf(w, `{"app":%q,"state":%q}`+"\n", app.Name, app.State)
	}
}

func (h *Handler) maintain(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	if app.Maintenance {
		h.unavailablePage(w, r, app, pages.Maintenance, maintenanceRetryAfter)
		return
	}
	next()
}

func (h *Handler) staticFiles(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	if serveStatic(w, r, app) {
		return
	}
	next()
}

func (h *Handler) bufferBody(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	bufferRequest(w, r, int64(app.Web.MaxBody), next)
}
