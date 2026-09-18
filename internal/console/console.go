package console

import (
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
	"regexp"
	"strconv"
	"strings"
	"time"

	"app-boss/internal/apps"
	"app-boss/internal/config"
	"app-boss/internal/logstore"
	"app-boss/internal/metrics"
	"app-boss/internal/ops"
	"app-boss/internal/super"
)

const maxRequestBody = 1 << 20
const maxHookBody = 1 << 20

//go:embed static/*
var assets embed.FS

// ConfigStore edits the config files appboss reads; apps.Store is the real one.
type ConfigStore interface {
	Files() ([]apps.ConfigFile, error)
	Read(id string) (apps.ConfigFile, error)
	Validate(id, contents string) error
	Write(id, contents, revision string) (apps.ConfigFile, error)
	CreateLocal(app string) (apps.ConfigFile, error)
	Effective(app string) (string, error)
}

type Handler struct {
	service        *ops.Service
	store          ConfigStore
	auth           *authenticator
	static         fs.FS
	managementPort string
	metricsEnabled bool
	metricsToken   string
	notifyStats    func() metrics.NotifyStats
}

type dashboard struct {
	Viewer          string           `json:"viewer"`
	CSRF            string           `json:"csrf"`
	Apps            []super.Snapshot `json:"apps"`
	RestartRequired []string         `json:"restart_required"`
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

func New(cfg config.Config, service *ops.Service, store ConfigStore, notifyStats func() metrics.NotifyStats) (*Handler, error) {
	auth, err := newAuthenticator(cfg)
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	// The console's own listener sits on the first port of the range, reserved by the allocator.
	return &Handler{service: service, store: store, auth: auth, static: static, managementPort: strconv.Itoa(cfg.Ports.Range[0]), metricsEnabled: cfg.Management.Metrics.Enabled, metricsToken: cfg.Management.Metrics.Token, notifyStats: notifyStats}, nil
}

// LoginURL mints a one-time link for `appboss login`. It points at the console's loopback
// listener, so it works without DNS and, through an SSH tunnel, from another machine.
func (h *Handler) LoginURL() (string, error) {
	token, err := h.auth.issueCLIToken()
	if err != nil {
		return "", err
	}
	link := url.URL{Scheme: "http", Host: "127.0.0.1:" + h.managementPort, Path: cliLoginPath, RawQuery: "token=" + url.QueryEscape(token)}
	return link.String(), nil
}

// consoleHost accepts the configured management hostname, which AuthCog can sign in, and the
// loopback names that only the CLI login can sign in.
func (h *Handler) consoleHost(rawHost string) bool {
	if _, err := authDestination(rawHost, h.auth.hosts); err == nil {
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
	case r.Method == http.MethodGet && r.URL.Path == "/logs":
		h.serveAsset(w, r, "log.html")
	case r.Method == http.MethodGet && r.URL.Path == "/logs.txt":
		h.writeLogs(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/assets/"):
		h.serveAsset(w, r, strings.TrimPrefix(r.URL.Path, "/assets/"))
	case r.Method == http.MethodGet && r.URL.Path == "/favicon.ico":
		h.serveFavicon(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/bootstrap":
		h.writeDashboard(w, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/apps":
		writeJSON(w, http.StatusOK, map[string]any{"apps": h.service.Apps(), "updated_at": time.Now().UTC()})
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
	case r.Method == http.MethodGet && r.URL.Path == "/api/config/keys":
		writeJSON(w, http.StatusOK, config.Keys())
	case r.Method == http.MethodGet && r.URL.Path == "/api/hooks":
		h.hooks(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/hooks/run":
		h.hookRun(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/hooks/rotate":
		h.hookRotate(w, r, session)
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
	writeJSON(w, http.StatusOK, dashboard{Viewer: session.Email, CSRF: session.CSRF, Apps: h.service.Apps(), RestartRequired: h.service.RestartRequired(), UpdatedAt: time.Now().UTC()})
}

func (h *Handler) configFiles(w http.ResponseWriter) {
	files, err := h.store.Files()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (h *Handler) configFile(w http.ResponseWriter, r *http.Request) {
	file, err := h.store.Read(r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, file)
}

func (h *Handler) configValidate(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var edit configEdit
	if err := decodeJSON(w, r, &edit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.Validate(edit.ID, edit.Contents); err != nil {
		writeJSON(w, http.StatusOK, validateResponse{Error: err.Error(), Line: yamlLine(err)})
		return
	}
	writeJSON(w, http.StatusOK, validateResponse{OK: true})
}

// configWrite saves the file and rescans right away, so the response carries what the change
// did: apps that became invalid and host keys that now wait for a restart.
func (h *Handler) configWrite(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var edit configEdit
	if err := decodeJSON(w, r, &edit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	file, err := h.store.Write(edit.ID, edit.Contents, edit.Revision)
	if errors.Is(err, apps.ErrConflict) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "file": file})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, validateResponse{Error: err.Error(), Line: yamlLine(err)})
		return
	}
	result, err := h.service.Rescan()
	if err != nil {
		writeError(w, http.StatusConflict, "saved, but rescan failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, writeResponse{File: file, Invalid: result.Invalid, RestartRequired: result.RestartRequired})
}

func (h *Handler) configLocal(w http.ResponseWriter, r *http.Request, session authSession) {
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
	file, err := h.store.CreateLocal(strings.TrimSpace(request.App))
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, file)
}

func (h *Handler) configEffective(w http.ResponseWriter, r *http.Request) {
	contents, err := h.store.Effective(strings.TrimSpace(r.URL.Query().Get("app")))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeText(w, contents)
}

// healthz is a liveness probe: 200 while the HTTP server answers.
func (h *Handler) healthz(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// readyz is 200 only while every autostart app is running, so a load balancer or uptime checker
// can hold traffic back during a startup.
func (h *Handler) readyz(w http.ResponseWriter) {
	var notReady []string
	for _, app := range h.service.Apps() {
		if app.Autostart && app.State != super.Running {
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
			w.Header().Set("WWW-Authenticate", `Bearer realm="appboss"`)
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
	_, _ = io.WriteString(w, metrics.Render(h.service.Apps(), time.Now(), stats))
}

// handleHook accepts a signed ping at /hooks/<app>/<hook> and starts the hook. It is the one
// console route outside the session flow: the Git host cannot carry a session, so the hook's own
// secret is the credential. The request body is read raw for the GitHub HMAC.
func (h *Handler) handleHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/hooks/")
	app, hookName, ok := strings.Cut(rest, "/")
	if !ok || app == "" || hookName == "" || strings.Contains(hookName, "/") {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
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
	if err := h.service.RunHook(app, hookName); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "app": app, "hook": hookName})
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
	if err := h.service.RunHook(request.App, request.Hook); err != nil {
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
	info, err := h.service.RotateHook(request.App, request.Hook)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hook": info})
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

var yamlLinePattern = regexp.MustCompile(`line (\d+)`)

// yamlLine finds the line an error points at so the editor can highlight it.
func yamlLine(err error) int {
	var cfgErr *config.Error
	if errors.As(err, &cfgErr) && cfgErr.Line > 0 {
		return cfgErr.Line
	}
	match := yamlLinePattern.FindStringSubmatch(err.Error())
	if match == nil {
		return 0
	}
	line, _ := strconv.Atoi(match[1])
	return line
}

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
	if _, err := h.service.Do(ops.Request{Method: method, App: request.App, On: on, Job: strings.TrimSpace(request.Job)}); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": h.service.Apps(), "updated_at": time.Now().UTC()})
}

// actionMethod maps the console's button names to the canonical ops actions. Maintenance is the
// only action with a second argument, so it gets its own names on the wire.
func actionMethod(action string) (string, bool, error) {
	switch action {
	case ops.ActionStart, ops.ActionStop, ops.ActionRestart:
		return action, false, nil
	case "maintenance-on":
		return ops.ActionMaintenance, true, nil
	case "maintenance-off":
		return ops.ActionMaintenance, false, nil
	case ops.ActionCronRun:
		return ops.ActionCronRun, false, nil
	default:
		return "", false, errors.New("action must be start, stop, restart, maintenance-on, maintenance-off, or cron-run")
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
			fmt.Fprintf(w, "%s %s %s %d %s %dms %dB %s\n", e.Time.Format(time.RFC3339), e.Method, e.Host, e.Status, e.Path, e.DurationMS, e.BytesOut, e.IP)
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
