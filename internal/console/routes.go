package console

import (
	"crypto/subtle"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"time"

	"dboss/internal/config"
	"dboss/internal/httpx"
	"dboss/internal/metrics"
	"dboss/internal/pages"
	"dboss/internal/version"
)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if !h.consoleHost(r.Host) {
		http.NotFound(w, r)
		return
	}
	h.mux.ServeHTTP(w, r)
}

// routes is the console's URL table. Hook pings, health and metrics come first and stay outside
// the session: a Git host, an uptime checker or Prometheus cannot hold one, so a hook ping and
// /metrics present tokens.dboss instead. Every other route signs the request in first.
func (h *Handler) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/", h.handleHook)
	mux.HandleFunc("GET /hooks/", h.handleHookStatus)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { h.healthz(w) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { h.readyz(w) })
	mux.HandleFunc("GET /metrics", h.metrics)
	mux.HandleFunc("GET "+pages.LogoPath, func(w http.ResponseWriter, _ *http.Request) { pages.ServeLogo(w) })
	session := func(pattern string, serve func(http.ResponseWriter, *http.Request, authSession)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if current, ok := h.auth.authenticate(w, r); ok {
				serve(w, r, current)
			}
		})
	}
	signedIn := func(pattern string, serve http.HandlerFunc) {
		session(pattern, func(w http.ResponseWriter, r *http.Request, _ authSession) { serve(w, r) })
	}
	// The sign-in paths (/login, the AuthCog callback) are answered inside authenticate, so an
	// unknown path still runs it: an anonymous visitor is sent to sign in, not told 404.
	signedIn("/", http.NotFound)
	signedIn("GET /{$}", func(w http.ResponseWriter, r *http.Request) { h.serveAsset(w, r, "index.html") })
	signedIn("GET /logs.txt", h.writeLogs)
	signedIn("GET /assets/{asset...}", func(w http.ResponseWriter, r *http.Request) { h.serveAsset(w, r, r.PathValue("asset")) })
	signedIn("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) { pages.ServeLogo(w) })
	session("GET /api/bootstrap", h.writeDashboard)
	signedIn("GET /api/apps", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"apps": h.service.Apps(), "capabilities": h.capabilities(), "updated_at": time.Now().UTC()})
	})
	session("POST /api/disk/refresh", h.diskRefresh)
	signedIn("GET /api/log/channels", h.logChannels)
	signedIn("GET /api/log/tree", h.logTree)
	signedIn("GET /api/log/search", h.logSearch)
	signedIn("GET /api/log/blocked", h.logBlocked)
	session("POST /api/action", h.action)
	session("POST /api/rescan", h.rescan)
	session("POST /api/apps/add", h.addApp)
	session("POST /api/logout", h.logout)
	signedIn("GET /api/config", h.configFiles)
	signedIn("GET /api/config/file", h.configFile)
	session("POST /api/config/validate", h.configValidate)
	session("PUT /api/config/file", h.configWrite)
	session("POST /api/config/local", h.configLocal)
	signedIn("GET /api/config/effective", h.configEffective)
	signedIn("GET /api/config/reference", func(w http.ResponseWriter, r *http.Request) { writeText(w, config.Reference) })
	signedIn("GET /api/config/history", h.configHistory)
	signedIn("GET /api/config/history/file", h.configHistoryFile)
	session("POST /api/config/restore", h.configRestore)
	signedIn("GET /api/config/keys", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, config.Keys()) })
	signedIn("GET /api/config/blocks", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, config.Blocks()) })
	signedIn("GET /api/config/form", h.configForm)
	session("POST /api/config/apply", h.configApply)
	signedIn("GET /api/hooks", h.hooks)
	session("POST /api/hooks/run", h.hookRun)
	signedIn("GET /api/traffic", h.traffic)
	signedIn("GET /api/traffic/fleet", h.fleetTraffic)
	signedIn("GET /api/audit", h.audit)
	signedIn("GET /api/sys", h.writeSys)
	session("POST /api/sys/refresh", h.sysRefresh)
	signedIn("GET /api/pg", h.writePG)
	session("POST /api/pg/refresh", h.pgRefresh)
	signedIn("GET /api/pg/backups", h.writePGBackups)
	session("POST /api/pg/backup", h.pgBackup)
	session("POST /api/pg/backup/delete", h.pgDeleteBackup)
	session("GET /api/pg/backup/download", h.pgDownloadBackup)
	session("POST /api/pg/backup/upload", h.pgUploadBackup)
	session("POST /api/pg/restore", h.pgRestore)
	session("POST /api/pg/drop", h.pgDrop)
	session("POST /api/pg/config", h.pgConfig)
	session("POST /api/pg/query", h.pgQuery)
	signedIn("GET /api/events", h.events)
	signedIn("GET /api/events/latest", h.eventsLatest)
	signedIn("GET /api/events/facets", h.eventsFacets)
	signedIn("GET /api/events/views", h.eventsViews)
	session("POST /api/events/funnel", h.eventsFunnel)
	session("POST /api/events/query", h.eventsQuery)
	session("POST /api/events/views", h.eventsSave)
	session("POST /api/events/views/delete", h.eventsDelete)
	signedIn("GET /api/pubsub", h.writePubsub)
	signedIn("GET /api/pubsub/secret", h.pubsubSecret)
	session("POST /api/pubsub/rotate", h.pubsubRotate)
	session("POST /api/pubsub/publish", h.pubsubPublish)
	return mux
}

// serveAsset answers from the embedded static folder. Fez component files (.fez) have no
// registered type and go out as plain text, which is all the runtime needs to fetch them.
func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(h.static, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) writeDashboard(w http.ResponseWriter, _ *http.Request, session authSession) {
	writeJSON(w, http.StatusOK, dashboard{Viewer: session.Email, CSRF: session.CSRF, Apps: h.service.Apps(), RestartRequired: h.service.RestartRequired(), Capabilities: h.capabilities(), Version: version.String(), Hostname: h.hostname, AppScheme: h.appScheme, AppPort: h.appPort, UpdatedAt: time.Now().UTC()})
}

// capabilities tells the shell which optional tabs to show; held raises the ENTER bar.
func (h *Handler) capabilities() map[string]bool {
	return map[string]bool{"postgres": h.service.PGAvailable(), "pubsub": len(h.service.PubsubApps()) > 0, "dev": h.dev, "held": !h.service.Booted()}
}

// healthz is a liveness probe: 200 while the HTTP server answers.
func (h *Handler) healthz(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// readyz is 200 only while every autostart app serves, so a load balancer or uptime checker can
// hold traffic back during a startup. An app put to sleep by idle_stop still counts as ready.
func (h *Handler) readyz(w http.ResponseWriter) {
	var notReady []string
	for _, app := range h.service.Apps() {
		if app.Autostart && !app.Serving() {
			notReady = append(notReady, app.Name+"="+string(app.State))
		}
	}
	if len(notReady) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "not_ready": notReady})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// metrics renders the Prometheus exposition for a bearer holding tokens.dboss. Without a token
// configured the endpoint does not exist.
func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	token := h.service.DbossToken()
	if token == "" {
		http.Error(w, "metrics are off: set tokens.dboss in the host dboss.yaml and send it as a bearer token", http.StatusNotFound)
		return
	}
	if subtle.ConstantTimeCompare([]byte(httpx.BearerToken(r)), []byte(token)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="dboss"`)
		http.Error(w, "forbidden", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, metrics.Render(h.service.Metrics()))
}
