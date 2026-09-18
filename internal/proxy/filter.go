package proxy

import (
	"fmt"
	"net/http"

	"app-boss/internal/super"
)

// Filter is one stage of the request pipeline. It either answers the request itself or calls
// next to continue. Stages run in the order they were added, so a module filter can slot in
// without disturbing the built-in ones.
type Filter func(w http.ResponseWriter, r *http.Request, app super.Snapshot, next func())

// serve runs the pipeline for an app ResolveHost already picked.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, app super.Snapshot) {
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
	h.filters = append(h.filters[:0], h.canonical, h.allow, h.publicHealth, h.authorize, h.maintain, h.staticFiles, h.bufferBody)
	h.filters = append(h.filters, extra...)
	h.filters = append(h.filters, func(w http.ResponseWriter, r *http.Request, app super.Snapshot, _ func()) {
		h.forward(w, r, app)
	})
}

// The built-in stages, in request order.

func (h *Handler) canonical(w http.ResponseWriter, r *http.Request, app super.Snapshot, next func()) {
	if redirectCanonical(w, r, app.CanonicalHost) {
		return
	}
	next()
}

func (h *Handler) allow(w http.ResponseWriter, r *http.Request, app super.Snapshot, next func()) {
	if !allowed(clientIP(r, h.cfg.Proxy.ClientIPHeaders), app.Web.AllowPrefixes()) {
		h.forbidden(w, r)
		return
	}
	next()
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, app super.Snapshot, next func()) {
	if !authorized(w, r, app) {
		return
	}
	next()
}

// publicHealth answers the app's own status path without auth, so a Cloudflare health check or
// uptime monitor can probe the app domain. It reports liveness, not readiness: 200 while the app
// runs and is not draining, 503 otherwise, and it never wakes a stopped app.
func (h *Handler) publicHealth(w http.ResponseWriter, r *http.Request, app super.Snapshot, next func()) {
	path := app.Web.HealthEndpoint
	if path == "" || r.URL.Path != path || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		next()
		return
	}
	status := http.StatusOK
	if app.State != super.Running || app.Draining {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method == http.MethodGet {
		fmt.Fprintf(w, `{"app":%q,"state":%q}`+"\n", app.Name, app.State)
	}
}

func (h *Handler) maintain(w http.ResponseWriter, r *http.Request, app super.Snapshot, next func()) {
	if app.Maintenance {
		h.unavailablePage(w, r, h.maintenancePage(app), app.Name, maintenanceRetryAfter)
		return
	}
	next()
}

func (h *Handler) staticFiles(w http.ResponseWriter, r *http.Request, app super.Snapshot, next func()) {
	if serveStatic(w, r, app) {
		return
	}
	next()
}

func (h *Handler) bufferBody(w http.ResponseWriter, r *http.Request, app super.Snapshot, next func()) {
	bufferRequest(w, r, int64(app.Web.MaxBody), next)
}
