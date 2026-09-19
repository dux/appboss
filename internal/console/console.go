package console

import (
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
	"regexp"
	"strconv"
	"strings"
	"time"

	"app-boss/internal/apps"
	"app-boss/internal/config"
	"app-boss/internal/logstore"
	"app-boss/internal/metrics"
	"app-boss/internal/ops"
	"app-boss/internal/pg"
	"app-boss/internal/pubsub"
	"app-boss/internal/super"
	"app-boss/internal/sysinfo"

	"gopkg.in/yaml.v3"
)

const maxRequestBody = 1 << 20
const maxHookBody = 1 << 20

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

// ConfigStore edits the config files appboss reads; apps.Store is the real one.
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
}

type dashboard struct {
	Viewer          string           `json:"viewer"`
	CSRF            string           `json:"csrf"`
	Apps            []super.Snapshot `json:"apps"`
	RestartRequired []string         `json:"restart_required"`
	Capabilities    map[string]bool  `json:"capabilities"`
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
	handler := &Handler{service: service, store: store, auth: auth, static: static, managementPort: strconv.Itoa(cfg.Ports.Range[0]), metricsEnabled: cfg.Management.Metrics.Enabled, metricsToken: cfg.Management.Metrics.Token, notifyStats: notifyStats, sys: sys}
	if len(cfg.Management.Host) > 0 {
		handler.publicHost = cfg.Management.Host[0]
	}
	return handler, nil
}

// LoginURL mints a one-time link for `appboss login`. It returns the console's loopback
// address, which works without DNS and through an SSH tunnel, and the public management host
// when one is configured. Both links carry the same single-use token.
func (h *Handler) LoginURL() (local, public string, err error) {
	token, err := h.auth.issueCLIToken()
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
		writeJSON(w, http.StatusOK, map[string]any{"apps": h.service.Apps(), "capabilities": h.capabilities(), "updated_at": time.Now().UTC()})
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
	case r.Method == http.MethodPost && r.URL.Path == "/api/pg/restore":
		h.pgRestore(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/pg/config":
		h.pgConfig(w, r, session)
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
	writeJSON(w, http.StatusOK, dashboard{Viewer: session.Email, CSRF: session.CSRF, Apps: h.service.Apps(), RestartRequired: h.service.RestartRequired(), Capabilities: h.capabilities(), UpdatedAt: time.Now().UTC()})
}

// capabilities tells the shell which optional tabs to show.
func (h *Handler) capabilities() map[string]bool {
	return map[string]bool{"postgres": h.service.PGAvailable(), "pubsub": len(h.service.PubsubApps()) > 0}
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
		h.service.Audit(session.Email, "", "config-write", edit.ID, err)
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "file": file})
		return
	}
	if err != nil {
		h.service.Audit(session.Email, "", "config-write", edit.ID, err)
		writeJSON(w, http.StatusUnprocessableEntity, validateResponse{Error: err.Error(), Line: yamlLine(err)})
		return
	}
	result, err := h.service.Rescan()
	if err != nil {
		h.service.Audit(session.Email, file.App, "config-write", file.ID, err)
		writeError(w, http.StatusConflict, "saved, but rescan failed: "+err.Error())
		return
	}
	h.service.Audit(session.Email, file.App, "config-write", file.ID, nil)
	if len(result.RestartRequired) > 0 {
		h.service.Notify("config-changed", file.App, "restart required: "+strings.Join(result.RestartRequired, ", "))
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
		h.service.Audit(session.Email, request.App, "config-local", request.App, err)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	h.service.Audit(session.Email, file.App, "config-local", file.ID, nil)
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

func (h *Handler) configHistory(w http.ResponseWriter, r *http.Request) {
	revisions, err := h.store.History(strings.TrimSpace(r.URL.Query().Get("id")))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": revisions})
}

func (h *Handler) configHistoryFile(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	contents, err := h.store.HistoryContents(strings.TrimSpace(query.Get("id")), strings.TrimSpace(query.Get("revision")))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeText(w, contents)
}

// configRestore writes a saved revision back and rescans, like a normal save.
func (h *Handler) configRestore(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var edit configEdit
	if err := decodeJSON(w, r, &edit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	file, err := h.store.Restore(edit.ID, edit.Revision)
	if errors.Is(err, apps.ErrConflict) {
		h.service.Audit(session.Email, "", "config-restore", edit.ID, err)
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "file": file})
		return
	}
	if err != nil {
		h.service.Audit(session.Email, "", "config-restore", edit.ID, err)
		writeJSON(w, http.StatusUnprocessableEntity, validateResponse{Error: err.Error(), Line: yamlLine(err)})
		return
	}
	result, err := h.service.Rescan()
	if err != nil {
		h.service.Audit(session.Email, file.App, "config-restore", file.ID, err)
		writeError(w, http.StatusConflict, "restored, but rescan failed: "+err.Error())
		return
	}
	h.service.Audit(session.Email, file.App, "config-restore", file.ID, nil)
	if len(result.RestartRequired) > 0 {
		h.service.Notify("config-changed", file.App, "restart required: "+strings.Join(result.RestartRequired, ", "))
	}
	writeJSON(w, http.StatusOK, writeResponse{File: file, Invalid: result.Invalid, RestartRequired: result.RestartRequired})
}

// configForm answers the visual editor: the active file, every value it sets (parsed raw, so a
// $VAR is shown as written), and the recipes for the file's role.
func (h *Handler) configForm(w http.ResponseWriter, r *http.Request) {
	file, err := h.store.Read(strings.TrimSpace(r.URL.Query().Get("id")))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	values, err := parseValues(file.Contents)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	scope := recipeScope(file)
	writeJSON(w, http.StatusOK, map[string]any{"file": file, "values": values, "recipes": recipesForScope(scope)})
}

// configApply is the form's save. It writes the recipe's keys into the server-only override
// (created from the base when missing), validates and rescans like a normal config write, and
// keeps only the paths the recipe declares so the endpoint cannot write an arbitrary key.
func (h *Handler) configApply(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		ID       string         `json:"id"`
		Revision string         `json:"revision"`
		Recipe   string         `json:"recipe"`
		Values   map[string]any `json:"values"`
		Reset    []string       `json:"reset"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	file, err := h.store.Read(request.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	scope := recipeScope(file)
	allowed, ok := recipePaths(request.Recipe, scope)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown %s recipe %q", scope, request.Recipe))
		return
	}
	values, reset := filterRecipeValues(request.Values, request.Reset, allowed)
	if _, err := h.ensureLocalOverride(file); err != nil {
		h.service.Audit(session.Email, file.App, "config-apply", file.ID, err)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if file, err = h.store.Read(request.ID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	contents, err := applyFormPatch(file.Contents, values, reset)
	if err != nil {
		h.service.Audit(session.Email, file.App, "config-apply", file.ID, err)
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	written, err := h.store.Write(request.ID, contents, request.Revision)
	if errors.Is(err, apps.ErrConflict) {
		h.service.Audit(session.Email, file.App, "config-apply", file.ID, err)
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "file": written})
		return
	}
	if err != nil {
		h.service.Audit(session.Email, file.App, "config-apply", file.ID, err)
		writeJSON(w, http.StatusUnprocessableEntity, validateResponse{Error: err.Error(), Line: yamlLine(err)})
		return
	}
	result, err := h.service.Rescan()
	if err != nil {
		h.service.Audit(session.Email, file.App, "config-apply", file.ID, err)
		writeError(w, http.StatusConflict, "saved, but rescan failed: "+err.Error())
		return
	}
	h.service.Audit(session.Email, file.App, "config-apply", file.ID, nil)
	if len(result.RestartRequired) > 0 {
		h.service.Notify("config-changed", file.App, "restart required: "+strings.Join(result.RestartRequired, ", "))
	}
	writeJSON(w, http.StatusOK, writeResponse{File: written, Invalid: result.Invalid, RestartRequired: result.RestartRequired})
}

// ensureLocalOverride puts the edit into appboss.local.yaml next to the active file, so a deploy
// never overwrites it. The host file and app files have their own helpers.
func (h *Handler) ensureLocalOverride(file apps.ConfigFile) (apps.ConfigFile, error) {
	if file.App != "" {
		return h.store.EnsureLocal(file.App)
	}
	host, ok := h.store.(HostConfigStore)
	if !ok {
		return apps.ConfigFile{}, errors.New("the host config override is not available")
	}
	return host.CreateHostLocal()
}

// recipeScope reports whether a file carries host keys or app keys.
func recipeScope(file apps.ConfigFile) string {
	if file.App != "" {
		return config.RecipeApp
	}
	return config.RecipeHost
}

// recipesForScope keeps the recipes that belong to a file's role.
func recipesForScope(scope string) []config.Recipe {
	var result []config.Recipe
	for _, recipe := range config.Recipes() {
		if recipe.Scope == scope {
			result = append(result, recipe)
		}
	}
	return result
}

// recipePaths lists the field paths one recipe may write, so a request cannot smuggle in a key
// the recipe does not own.
func recipePaths(id, scope string) (map[string]bool, bool) {
	for _, recipe := range config.Recipes() {
		if recipe.ID != id || recipe.Scope != scope {
			continue
		}
		paths := make(map[string]bool, len(recipe.Fields))
		for _, field := range recipe.Fields {
			paths[field.Path] = true
		}
		return paths, true
	}
	return nil, false
}

// filterRecipeValues keeps only the declared paths. A blank string, empty list or empty map
// means "use the default", so it moves from values to reset and the key is removed from YAML.
func filterRecipeValues(values map[string]any, reset []string, allowed map[string]bool) (map[string]any, []string) {
	clean := make(map[string]any, len(values))
	for path, value := range values {
		if !allowed[path] {
			continue
		}
		if emptyValue(value) {
			reset = append(reset, path)
			continue
		}
		clean[path] = value
	}
	seen := map[string]bool{}
	kept := make([]string, 0, len(reset))
	for _, path := range reset {
		if allowed[path] && !seen[path] {
			seen[path] = true
			kept = append(kept, path)
		}
	}
	return clean, kept
}

func emptyValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	}
	return false
}

// parseValues decodes a config file into the generic shape the form reads by dotted path. It
// parses the raw contents, so a $VAR stays literal and is never expanded into a secret.
func parseValues(contents string) (map[string]any, error) {
	values := map[string]any{}
	if strings.TrimSpace(contents) == "" {
		return values, nil
	}
	if err := yaml.Unmarshal([]byte(contents), &values); err != nil {
		return nil, err
	}
	return values, nil
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
		if app.Autostart && (app.State != super.Running || app.Draining) {
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
func (h *Handler) pgStats() metrics.PGStats {
	var stats metrics.PGStats
	snapshot, err := h.service.PGSnapshot(false)
	if err != nil {
		return stats
	}
	stats.Up = snapshot.Available
	for _, database := range snapshot.Databases {
		stats.Databases = append(stats.Databases, metrics.PGDatabase{Name: database.Name, SizeBytes: database.SizeBytes})
	}
	for _, entry := range h.service.Backups() {
		moment, _ := time.Parse(time.RFC3339, entry.Time)
		stats.Backups = append(stats.Backups, metrics.PGBackup{Database: entry.Database, Time: moment, Status: entry.Status})
	}
	return stats
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
	if _, err := h.service.Do(ops.Request{Method: ops.ActionHookRun, App: app, Hook: hookName, Actor: "hook:" + app + "/" + hookName}); err != nil {
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

// pubsubSecret returns one app's effective publish secret and example URLs.
func (h *Handler) pubsubSecret(w http.ResponseWriter, r *http.Request) {
	info, err := h.service.PubsubSecret(strings.TrimSpace(r.URL.Query().Get("app")))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// pubsubRotate mints a new generated secret for an app.
func (h *Handler) pubsubRotate(w http.ResponseWriter, r *http.Request, session authSession) {
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
	info, err := h.service.PubsubRotate(strings.TrimSpace(request.App))
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
		Channel string          `json:"channel"`
		Event   string          `json:"event"`
		Data    json.RawMessage `json:"data"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.service.Do(ops.Request{Method: ops.ActionPubsubPublish, App: strings.TrimSpace(request.App), Channel: strings.TrimSpace(request.Channel), Event: strings.TrimSpace(request.Event), Data: request.Data, Actor: session.Email})
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

// writeSys returns the cached host inspection for the console's Sys tab.
func (h *Handler) writeSys(w http.ResponseWriter) {
	if h.sys == nil {
		writeError(w, http.StatusNotFound, "system inspection is not available")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": h.sys.Snapshot(), "updated_at": time.Now().UTC()})
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
func (h *Handler) writePG(w http.ResponseWriter) {
	snapshot, err := h.service.PGSnapshot(false)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot, "backups": h.service.Backups(), "backup": h.service.PGBackupConfig(), "s3": h.service.S3Configured(), "updated_at": time.Now().UTC()})
}

// pgRefresh re-inspects the server on demand. It is read-only, so it writes no audit row.
func (h *Handler) pgRefresh(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	snapshot, err := h.service.PGSnapshot(true)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot, "backups": h.service.Backups(), "backup": h.service.PGBackupConfig(), "s3": h.service.S3Configured(), "updated_at": time.Now().UTC()})
}

func (h *Handler) writePGBackups(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"backups": h.service.Backups(), "updated_at": time.Now().UTC()})
}

func (h *Handler) pgBackup(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		Database string `json:"database"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.service.Do(ops.Request{Method: ops.ActionPGBackup, Database: strings.TrimSpace(request.Database), Actor: session.Email})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result, "backups": h.service.Backups(), "updated_at": time.Now().UTC()})
}

func (h *Handler) pgRestore(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request pg.RestoreRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.service.Do(ops.Request{Method: ops.ActionPGRestore, BackupID: strings.TrimSpace(request.ID), Target: strings.TrimSpace(request.Target), Replace: request.Replace, Confirm: request.Confirm, Actor: session.Email})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// pgConfig writes the backup policy and database selection into the server-only host override
// and hot-reloads the PostgreSQL service, so the checkboxes apply without a daemon restart.
func (h *Handler) pgConfig(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		Backup config.PostgresBackup `json:"backup"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	host, ok := h.store.(HostConfigStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "the host config override is not available")
		return
	}
	if _, err := host.CreateHostLocal(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	file, err := h.store.Read("host")
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	contents, err := patchPostgresBackup(file.Contents, request.Backup)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	written, err := h.store.Write("host", contents, file.Revision)
	if err != nil {
		h.service.Audit(session.Email, "", "pg-config", file.ID, err)
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if cfg, err := host.HostConfig(); err == nil {
		h.service.ApplyPGConfig(cfg)
	}
	h.service.Audit(session.Email, "", "pg-config", written.ID, nil)
	snapshot, _ := h.service.PGSnapshot(false)
	writeJSON(w, http.StatusOK, map[string]any{"file": written, "snapshot": snapshot, "backups": h.service.Backups(), "backup": h.service.PGBackupConfig(), "s3": h.service.S3Configured(), "updated_at": time.Now().UTC()})
}

// patchPostgresBackup replaces the postgres.backup mapping in a host config, leaving every other
// key untouched. Comments in the file are normalized, since the override is machine-managed.
func patchPostgresBackup(contents string, backup config.PostgresBackup) (string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(contents), &root); err != nil {
		return "", err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return "", errors.New("host config must be a YAML mapping")
	}
	document := root.Content[0]
	postgres := ensureMapping(document, "postgres")
	var node yaml.Node
	if err := node.Encode(backup); err != nil {
		return "", err
	}
	setMapping(postgres, "backup", &node)
	out, err := yaml.Marshal(&root)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func ensureMapping(parent *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			if parent.Content[i+1].Kind != yaml.MappingNode {
				parent.Content[i+1] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}
			return parent.Content[i+1]
		}
	}
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, node)
	return node
}

func setMapping(parent *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			parent.Content[i+1] = value
			return
		}
	}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
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
