package console

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"dboss/internal/apps"
	"dboss/internal/authcog"
	"dboss/internal/config"
	"dboss/internal/logstore"
	"dboss/internal/logx"
	"dboss/internal/metrics"
	"dboss/internal/ops"
	"dboss/internal/pubsub"
	"dboss/internal/super"
	"dboss/internal/sysinfo"
	"dboss/internal/version"
)

const maxRequestBody = 1 << 20
const maxHookBody = 1 << 20

// maxBackupUpload bounds an uploaded dump. A database archive has nothing to do with the JSON
// body cap, and a plain-SQL dump zips down far below this.
const maxBackupUpload = 4 << 30

//go:embed static/*
var assets embed.FS

// SysReader is the read-only host inspection behind the Sys tab. sysinfo.Inspector is the real
// one; both methods return the current snapshot, and Refresh re-samples the host.
type SysReader interface {
	Snapshot() sysinfo.Snapshot
	Refresh(context.Context) sysinfo.Snapshot
}

// HostConfigStore is the subset of apps.Store the PostgreSQL config writer needs: create the
// server-only override and reload the active host config afterwards. A store without it disables
// the in-console backup settings.
type HostConfigStore interface {
	CreateHostLocal() (apps.ConfigFile, error)
	HostConfig() (config.Config, error)
}

// ConfigStore edits the config files dboss reads; apps.Store is the real one.
type ConfigStore interface {
	Files() ([]apps.ConfigFile, error)
	Read(id string) (apps.ConfigFile, error)
	Validate(id, contents string) error
	Write(id, contents, revision string) (apps.ConfigFile, error)
	CreateLocal(app string) (apps.ConfigFile, error)
	EnsureLocal(app string) (apps.ConfigFile, error)
	Effective(app string) (string, error)
	History(id string) ([]apps.ConfigRevision, error)
	HistoryContents(id, revision string) (string, error)
	Restore(id, revision string) (apps.ConfigFile, error)
}

type Handler struct {
	service        *ops.Service
	store          ConfigStore
	auth           *authenticator
	static         fs.FS
	managementPort string
	publicHost     string
	metricsEnabled bool
	metricsToken   string
	notifyStats    func() metrics.NotifyStats
	sys            SysReader
	dev            bool
}

type dashboard struct {
	Viewer          string           `json:"viewer"`
	CSRF            string           `json:"csrf"`
	Apps            []super.Snapshot `json:"apps"`
	RestartRequired []string         `json:"restart_required"`
	Capabilities    map[string]bool  `json:"capabilities"`
	Version         string           `json:"version"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

type configEdit struct {
	ID       string `json:"id"`
	Contents string `json:"contents"`
	Revision string `json:"revision,omitempty"`
}

type validateResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Line  int    `json:"line,omitempty"`
}

type writeResponse struct {
	File            apps.ConfigFile `json:"file"`
	Invalid         []string        `json:"invalid"`
	RestartRequired []string        `json:"restart_required"`
}

type actionRequest struct {
	App    string `json:"app"`
	Action string `json:"action"`
	Job    string `json:"job,omitempty"`
}

func New(cfg config.Config, service *ops.Service, store ConfigStore, notifyStats func() metrics.NotifyStats, sys SysReader) (*Handler, error) {
	auth, err := newAuthenticator(cfg)
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	// The console's own listener sits on the first port of the range, reserved by the allocator.
	handler := &Handler{service: service, store: store, auth: auth, static: static, managementPort: strconv.Itoa(cfg.Ports.Range[0]), metricsEnabled: cfg.Management.Metrics.Enabled, metricsToken: cfg.Management.Metrics.Token, notifyStats: notifyStats, sys: sys, dev: cfg.Dev()}
	if len(cfg.Management.Host) > 0 {
		handler.publicHost = cfg.Management.Host[0]
	}
	return handler, nil
}

// LoginURL mints a one-time link for `dboss login`. It returns the console's loopback
// address, which works without DNS and through an SSH tunnel, and the public management host
// when one is configured. Both links carry the same single-use token.
func (h *Handler) LoginURL() (local, public string, err error) {
	return h.loginURL(h.auth.issueCLIToken)
}

// DevLoginURL is the loopback link printed in the startup banner of a hand-run session: it lives
// for an hour and survives being clicked, so the one line stays useful for the whole session.
// Callers must gate it on the session being a terminal; it is never minted under systemd.
func (h *Handler) DevLoginURL() (string, error) {
	local, _, err := h.loginURL(h.auth.issueDevToken)
	return local, err
}

func (h *Handler) loginURL(issue func() (string, error)) (local, public string, err error) {
	token, err := issue()
	if err != nil {
		return "", "", err
	}
	query := "token=" + url.QueryEscape(token)
	link := url.URL{Scheme: "http", Host: "127.0.0.1:" + h.managementPort, Path: cliLoginPath, RawQuery: query}
	local = link.String()
	if h.publicHost != "" {
		link.Scheme = "https"
		link.Host = h.publicHost
		public = link.String()
	}
	return local, public, nil
}

// consoleHost accepts the configured management hostname, which AuthCog can sign in, and the
// loopback names that only the CLI login can sign in.
func (h *Handler) consoleHost(rawHost string) bool {
	if _, err := authcog.Destination(rawHost, h.auth.gate.Hosts); err == nil {
		return true
	}
	return loopbackHost(rawHost)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if !h.consoleHost(r.Host) {
		http.NotFound(w, r)
		return
	}
	// Hook pings carry their own secret, not a console session, so they are handled first.
	if strings.HasPrefix(r.URL.Path, "/hooks/") {
		h.handleHook(w, r)
		return
	}
	// Health and metrics are open on purpose: an uptime checker or Prometheus cannot hold a
	// console session. /metrics takes a bearer token when one is configured.
	if h.metricsEnabled {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			h.healthz(w)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/readyz":
			h.readyz(w)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/metrics":
			h.metrics(w, r)
			return
		}
	}
	session, ok := h.auth.authenticate(w, r)
	if !ok {
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		h.serveAsset(w, r, "index.html")
	case r.Method == http.MethodGet && r.URL.Path == "/logs.txt":
		h.writeLogs(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/assets/"):
		h.serveAsset(w, r, strings.TrimPrefix(r.URL.Path, "/assets/"))
	case r.Method == http.MethodGet && r.URL.Path == "/favicon.ico":
		h.serveFavicon(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/bootstrap":
		h.writeDashboard(w, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/apps":
		writeJSON(w, http.StatusOK, map[string]any{"apps": h.service.Apps(), "capabilities": h.capabilities(), "updated_at": time.Now().UTC()})
	case r.Method == http.MethodPost && r.URL.Path == "/api/disk/refresh":
		h.diskRefresh(w, r, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/log/channels":
		h.logChannels(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/log/tree":
		h.logTree(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/log/search":
		h.logSearch(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/action":
		h.action(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/rescan":
		h.rescan(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/logout":
		h.logout(w, r, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/config":
		h.configFiles(w)
	case r.Method == http.MethodGet && r.URL.Path == "/api/config/file":
		h.configFile(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/config/validate":
		h.configValidate(w, r, session)
	case r.Method == http.MethodPut && r.URL.Path == "/api/config/file":
		h.configWrite(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/config/local":
		h.configLocal(w, r, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/config/effective":
		h.configEffective(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/config/reference":
		writeText(w, config.Reference)
	case r.Method == http.MethodGet && r.URL.Path == "/api/config/history":
		h.configHistory(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/config/history/file":
		h.configHistoryFile(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/config/restore":
		h.configRestore(w, r, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/config/keys":
		writeJSON(w, http.StatusOK, config.Keys())
	case r.Method == http.MethodGet && r.URL.Path == "/api/config/blocks":
		writeJSON(w, http.StatusOK, config.Blocks())
	case r.Method == http.MethodGet && r.URL.Path == "/api/config/form":
		h.configForm(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/config/apply":
		h.configApply(w, r, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/hooks":
		h.hooks(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/hooks/run":
		h.hookRun(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/hooks/rotate":
		h.hookRotate(w, r, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/traffic":
		h.traffic(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/audit":
		h.audit(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/sys":
		h.writeSys(w)
	case r.Method == http.MethodPost && r.URL.Path == "/api/sys/refresh":
		h.sysRefresh(w, r, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/pg":
		h.writePG(w)
	case r.Method == http.MethodPost && r.URL.Path == "/api/pg/refresh":
		h.pgRefresh(w, r, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/pg/backups":
		h.writePGBackups(w)
	case r.Method == http.MethodPost && r.URL.Path == "/api/pg/backup":
		h.pgBackup(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/pg/backup/delete":
		h.pgDeleteBackup(w, r, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/pg/backup/download":
		h.pgDownloadBackup(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/pg/backup/upload":
		h.pgUploadBackup(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/pg/restore":
		h.pgRestore(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/pg/drop":
		h.pgDrop(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/pg/config":
		h.pgConfig(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/pg/query":
		h.pgQuery(w, r, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/pubsub":
		h.writePubsub(w)
	case r.Method == http.MethodGet && r.URL.Path == "/api/pubsub/secret":
		h.pubsubSecret(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/pubsub/rotate":
		h.pubsubRotate(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/pubsub/publish":
		h.pubsubPublish(w, r, session)
	default:
		http.NotFound(w, r)
	}
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

// serveFavicon answers the browser's default /favicon.ico request with the SVG mark, so no
// request escapes to a 404 before the <link rel="icon"> is read.
func (h *Handler) serveFavicon(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(h.static, "favicon.svg")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) writeDashboard(w http.ResponseWriter, session authSession) {
	writeJSON(w, http.StatusOK, dashboard{Viewer: session.Email, CSRF: session.CSRF, Apps: h.service.Apps(), RestartRequired: h.service.RestartRequired(), Capabilities: h.capabilities(), Version: version.String(), UpdatedAt: time.Now().UTC()})
}

// capabilities tells the shell which optional tabs to show.
func (h *Handler) capabilities() map[string]bool {
	return map[string]bool{"postgres": h.service.PGAvailable(), "pubsub": len(h.service.PubsubApps()) > 0, "dev": h.dev}
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

// metrics renders the Prometheus exposition. A configured token must match as a bearer token.
func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	if h.metricsToken != "" {
		authorization := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authorization, "Bearer ")
		if token == authorization || subtle.ConstantTimeCompare([]byte(token), []byte(h.metricsToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dboss"`)
			http.Error(w, "forbidden", http.StatusUnauthorized)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	stats := metrics.NotifyStats{}
	if h.notifyStats != nil {
		stats = h.notifyStats()
	}
	apps := h.service.Apps()
	latency := map[string]metrics.Latency{}
	for _, app := range apps {
		if value, err := h.service.Latency(app.Name); err == nil {
			latency[app.Name] = metrics.Latency{Count: value.Count, P50: value.P50, P95: value.P95, P99: value.P99}
		}
	}
	pubsub := map[string]metrics.PubsubStats{}
	for app, stats := range h.service.PubsubStats() {
		pubsub[app] = metrics.PubsubStats{Clients: stats.Clients, Channels: stats.Channels, Messages: stats.Messages}
	}
	_, _ = io.WriteString(w, metrics.Render(apps, time.Now(), stats, latency, h.pgStats(), pubsub))
}

// pgStats reduces the cached PostgreSQL inspection and backup catalog for the metrics output.
func (h *Handler) handleHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/hooks/")
	parts := strings.Split(rest, "/")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	// One segment is a host hook (the github_pr built-in); two are an app hook.
	if len(parts) == 1 && parts[0] != "" {
		h.handleHostHook(w, r, parts[0], body)
		return
	}
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	app, hookName := parts[0], parts[1]
	secret, err := h.service.HookSecret(app, hookName)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !hookAuthorized(r, secret, body) {
		http.Error(w, "forbidden", http.StatusUnauthorized)
		return
	}
	// GitHub sends a ping event when the webhook is created; acknowledge it without running.
	if r.Header.Get("X-GitHub-Event") == "ping" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "event": "ping", "app": app, "hook": hookName})
		return
	}
	if _, err := h.service.Do(ops.Request{Method: ops.ActionHookRun, App: app, Hook: hookName, Actor: "hook:" + app + "/" + hookName}); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "app": app, "hook": hookName})
}

// handleHostHook runs a host-level hook (the github_pr built-in). It answers 202 at once and
// deploys in the background, since a checkout and setup can take minutes.
func (h *Handler) handleHostHook(w http.ResponseWriter, r *http.Request, name string, body []byte) {
	secret, err := h.service.HostHookSecret(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !hookAuthorized(r, secret, body) {
		http.Error(w, "forbidden", http.StatusUnauthorized)
		return
	}
	params := qsParams(r)
	go func() {
		if _, err := h.service.Do(ops.Request{Method: ops.ActionHostHookRun, Hook: name, Params: params, Actor: "hook:" + name}); err != nil {
			logx.Warnf("host hook %s: %v", name, err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "hook": name})
}

// qsParams flattens the query into QS_<NAME> entries, skipping the hook's own token so a secret
// never reaches the hook process.
func qsParams(r *http.Request) map[string]string {
	query := r.URL.Query()
	params := make(map[string]string, len(query))
	for key, values := range query {
		if key == "token" || len(values) == 0 {
			continue
		}
		params["QS_"+qsEnvName(key)] = values[0]
	}
	return params
}

func qsEnvName(key string) string {
	var b strings.Builder
	for _, c := range strings.ToUpper(key) {
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

// hookAuthorized accepts either a GitHub HMAC signature over the raw body or a bearer token in
// the query, Authorization header or X-Gitlab-Token. Every comparison is constant time.
func hookAuthorized(r *http.Request, secret string, body []byte) bool {
	if signature := r.Header.Get("X-Hub-Signature-256"); strings.HasPrefix(signature, "sha256=") {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		expected := hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(expected), []byte(strings.TrimPrefix(signature, "sha256=")))
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		if authorization := r.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") {
			token = strings.TrimPrefix(authorization, "Bearer ")
		}
	}
	if token == "" {
		token = r.Header.Get("X-Gitlab-Token")
	}
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
}

func (h *Handler) hooks(w http.ResponseWriter, r *http.Request) {
	app := strings.TrimSpace(r.URL.Query().Get("app"))
	if app == "" {
		writeError(w, http.StatusBadRequest, "app is required")
		return
	}
	infos, err := h.service.Hooks(app)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hooks": infos, "updated_at": time.Now().UTC()})
}

func (h *Handler) hookRun(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	request, ok := h.decodeHookAction(w, r)
	if !ok {
		return
	}
	if _, err := h.service.Do(ops.Request{Method: ops.ActionHookRun, App: request.App, Hook: request.Hook, Actor: session.Email}); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) hookRotate(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	request, ok := h.decodeHookAction(w, r)
	if !ok {
		return
	}
	result, err := h.service.Do(ops.Request{Method: ops.ActionHookRotate, App: request.App, Hook: request.Hook, Actor: session.Email})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hook": result})
}

type hookAction struct {
	App  string `json:"app"`
	Hook string `json:"hook"`
}

func (h *Handler) decodeHookAction(w http.ResponseWriter, r *http.Request) (hookAction, bool) {
	var request hookAction
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return request, false
	}
	request.App, request.Hook = strings.TrimSpace(request.App), strings.TrimSpace(request.Hook)
	if request.App == "" || request.Hook == "" {
		writeError(w, http.StatusBadRequest, "app and hook are required")
		return request, false
	}
	return request, true
}

// writePubsub returns the realtime tab: every app that serves channels plus the integration guide.
func (h *Handler) writePubsub(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"apps": h.service.PubsubApps(), "guide": pubsub.Help, "updated_at": time.Now().UTC()})
}

// pubsubSecret returns one web process's effective publish secret and example URLs.
func (h *Handler) pubsubSecret(w http.ResponseWriter, r *http.Request) {
	info, err := h.service.PubsubSecret(strings.TrimSpace(r.URL.Query().Get("app")), strings.TrimSpace(r.URL.Query().Get("process")))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// pubsubRotate mints a new generated secret for a web process.
func (h *Handler) pubsubRotate(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		App     string `json:"app"`
		Process string `json:"process"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := h.service.PubsubRotate(strings.TrimSpace(request.App), strings.TrimSpace(request.Process))
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// pubsubPublish sends a message from the console's test box.
func (h *Handler) pubsubPublish(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		App     string          `json:"app"`
		Process string          `json:"process"`
		Channel string          `json:"channel"`
		Event   string          `json:"event"`
		Data    json.RawMessage `json:"data"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.service.Do(ops.Request{Method: ops.ActionPubsubPublish, App: strings.TrimSpace(request.App), Process: strings.TrimSpace(request.Process), Channel: strings.TrimSpace(request.Channel), Event: strings.TrimSpace(request.Event), Data: request.Data, Actor: session.Email})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// audit answers the console Audit tab and the CLI with the newest operator actions.
func (h *Handler) audit(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	rows, err := h.service.SearchAudit(logstore.AuditFilter{
		App:    strings.TrimSpace(query.Get("app")),
		Actor:  strings.TrimSpace(query.Get("actor")),
		Action: strings.TrimSpace(query.Get("action")),
		Limit:  limitParam(query.Get("limit"), 200),
	})
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "updated_at": time.Now().UTC()})
}

// trafficRanges are the spans the Traffic tab offers. A fixed set keeps every aggregate bounded.
var trafficRanges = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

// traffic returns the aggregated request log of one app. It is read-only, so it writes no audit row.
func (h *Handler) traffic(w http.ResponseWriter, r *http.Request) {
	app := strings.TrimSpace(r.URL.Query().Get("app"))
	if app == "" {
		writeError(w, http.StatusBadRequest, "app is required")
		return
	}
	span, ok := trafficRanges[r.URL.Query().Get("range")]
	if !ok {
		writeError(w, http.StatusBadRequest, "range must be 1h, 24h, 7d or 30d")
		return
	}
	traffic, err := h.service.Traffic(app, time.Now().Add(-span))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"traffic": traffic, "updated_at": time.Now().UTC()})
}

// writeSys returns the cached host inspection for the console's Sys tab.
func (h *Handler) writeSys(w http.ResponseWriter) {
	if h.sys == nil {
		writeError(w, http.StatusNotFound, "system inspection is not available")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": h.sys.Snapshot(), "updated_at": time.Now().UTC()})
}

// diskRefresh measures one app's footprint now instead of waiting for the daily pass. It only
// reads the filesystem, so it writes no audit row.
func (h *Handler) diskRefresh(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		App string `json:"app"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	app := strings.TrimSpace(request.App)
	if app == "" {
		writeError(w, http.StatusBadRequest, "app is required")
		return
	}
	if _, err := h.service.DiskRefresh(app); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": h.service.Apps(), "updated_at": time.Now().UTC()})
}

// sysRefresh re-samples the host and re-probes the toolchains on demand. It is read-only, so it
// writes no audit row.
func (h *Handler) sysRefresh(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	if h.sys == nil {
		writeError(w, http.StatusNotFound, "system inspection is not available")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": h.sys.Refresh(r.Context()), "updated_at": time.Now().UTC()})
}

// writePG returns the cached PostgreSQL inspection and the backup catalog.

func (h *Handler) action(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request actionRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.App = strings.TrimSpace(request.App)
	if request.App == "" {
		writeError(w, http.StatusBadRequest, "app is required")
		return
	}
	method, on, err := actionMethod(request.Action)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := h.service.Do(ops.Request{Method: method, App: request.App, On: on, Job: strings.TrimSpace(request.Job), Actor: session.Email}); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": h.service.Apps(), "updated_at": time.Now().UTC()})
}

// actionMethod maps the console's button names to the canonical ops actions. Maintenance is the
// only action with a second argument, so it gets its own names on the wire.
func actionMethod(action string) (string, bool, error) {
	switch action {
	case ops.ActionStart, ops.ActionStop, ops.ActionRestart, ops.ActionDestroy:
		return action, false, nil
	case "maintenance-on":
		return ops.ActionMaintenance, true, nil
	case "maintenance-off":
		return ops.ActionMaintenance, false, nil
	case ops.ActionCronRun:
		return ops.ActionCronRun, false, nil
	default:
		return "", false, errors.New("action must be start, stop, restart, destroy, maintenance-on, maintenance-off, or cron-run")
	}
}

func (h *Handler) rescan(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	result, err := h.service.Rescan()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// writeLogs streams the matching rows as plain text, for download and for `curl` against the
// store. It is the same query the viewer runs, without the pagination.
func (h *Handler) writeLogs(w http.ResponseWriter, r *http.Request) {
	app := strings.TrimSpace(r.URL.Query().Get("app"))
	if app == "" {
		http.Error(w, "app is required", http.StatusBadRequest)
		return
	}
	channel := channelParam(r)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if process, ok := requestProcess(channel); ok {
		filter := requestFilter(r, 5000)
		if process != "" {
			filter.Process = process
		}
		entries, err := h.service.SearchRequests(app, filter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		for _, e := range entries {
			fmt.Fprintf(w, "%s %s %s %d %s %dms %dB %s %s\n", e.Time.Format(time.RFC3339), e.Method, e.Host, e.Status, e.Path, e.DurationMS, e.BytesOut, e.IP, cmp.Or(e.Country, "-"))
		}
		return
	}
	entries, err := h.service.SearchLogs(app, logFilter(r, channel, 5000))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	for _, e := range entries {
		fmt.Fprintf(w, "%s %-5s %s %s\n", e.Time.Format(time.RFC3339), strings.ToUpper(e.Level), e.Process, e.Message)
	}
}

func (h *Handler) logChannels(w http.ResponseWriter, r *http.Request) {
	app := strings.TrimSpace(r.URL.Query().Get("app"))
	if app == "" {
		writeError(w, http.StatusBadRequest, "app is required")
		return
	}
	channels, err := h.service.Channels(app)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": channels, "updated_at": time.Now().UTC()})
}

func (h *Handler) logTree(w http.ResponseWriter, r *http.Request) {
	apps, err := h.service.LogTree()
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps, "updated_at": time.Now().UTC()})
}

// logSearch answers one viewer query. The request channel reads the requests table, every other
// channel reads the log rows, so the UI can treat them as one list.
func (h *Handler) logSearch(w http.ResponseWriter, r *http.Request) {
	app := strings.TrimSpace(r.URL.Query().Get("app"))
	if app == "" {
		writeError(w, http.StatusBadRequest, "app is required")
		return
	}
	if process, ok := requestProcess(channelParam(r)); ok {
		filter := requestFilter(r, 200)
		if process != "" {
			filter.Process = process
		}
		entries, err := h.service.SearchRequests(app, filter)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"kind": "request", "rows": entries, "updated_at": time.Now().UTC()})
		return
	}
	entries, err := h.service.SearchLogs(app, logFilter(r, channelParam(r), 200))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": "log", "rows": entries, "updated_at": time.Now().UTC()})
}

func channelParam(r *http.Request) string {
	channel := strings.TrimSpace(r.URL.Query().Get("channel"))
	if channel == "" {
		return "stdout"
	}
	return channel
}

func logFilter(r *http.Request, channel string, fallback int) logstore.LogFilter {
	query := r.URL.Query()
	return logstore.LogFilter{
		Channel: channel,
		Process: strings.TrimSpace(query.Get("process")),
		Level:   strings.TrimSpace(query.Get("level")),
		Query:   strings.TrimSpace(query.Get("q")),
		Since:   sinceParam(query.Get("since")),
		Before:  sinceParam(query.Get("before")),
		Limit:   limitParam(query.Get("limit"), fallback),
	}
}

func requestFilter(r *http.Request, fallback int) logstore.RequestFilter {
	query := r.URL.Query()
	status, class := statusParam(query.Get("status"))
	return logstore.RequestFilter{
		Method:      strings.TrimSpace(query.Get("method")),
		Process:     strings.TrimSpace(query.Get("process")),
		Status:      status,
		StatusClass: class,
		Query:       strings.TrimSpace(query.Get("q")),
		Since:       sinceParam(query.Get("since")),
		Before:      sinceParam(query.Get("before")),
		Limit:       limitParam(query.Get("limit"), fallback),
	}
}

// requestProcess reads a request channel id: "request" means every service, "request:<process>"
// means one.
func requestProcess(channel string) (string, bool) {
	if channel == "request" {
		return "", true
	}
	if name, ok := strings.CutPrefix(channel, "request:"); ok {
		return name, true
	}
	return "", false
}

// statusParam reads "404" as an exact code and "4xx" as a class.
func statusParam(value string) (int, int) {
	value = strings.TrimSpace(value)
	if len(value) == 3 && value[1] == 'x' && value[2] == 'x' && value[0] >= '2' && value[0] <= '5' {
		return 0, int(value[0]-'0') * 100
	}
	return intParam(value), 0
}

func sinceParam(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func intParam(value string) int {
	parsed, _ := strconv.Atoi(value)
	return parsed
}

func limitParam(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	if parsed > 5000 {
		return 5000
	}
	return parsed
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	h.auth.clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) requireCSRF(w http.ResponseWriter, r *http.Request, session authSession) bool {
	if h.auth.validCSRF(r, session) {
		return true
	}
	writeError(w, http.StatusForbidden, "invalid CSRF token")
	return false
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func secureHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	// Fez compiles components with new Function, wires template handlers as inline on* attributes
	// and injects scoped CSS as <style> nodes, so script and style need the unsafe-* sources.
	// Every origin other than the console itself stays blocked.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; connect-src 'self'; font-src 'self'; form-action 'self'; frame-ancestors 'none'; img-src 'self'; object-src 'none'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func writeText(w http.ResponseWriter, contents string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, contents)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
