package console

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"deploy-boss/internal/apps"
	"deploy-boss/internal/config"
	"deploy-boss/internal/logstore"
	"deploy-boss/internal/ops"
	"deploy-boss/internal/super"
)

const maxRequestBody = 1 << 20

//go:embed static/*
var assets embed.FS

// ConfigStore edits the config files dboss reads; apps.Store is the real one.
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
}

func New(cfg config.Config, service *ops.Service, store ConfigStore) (*Handler, error) {
	auth, err := newAuthenticator(cfg)
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	// The console's own listener sits on the first port of the range, reserved by the allocator.
	return &Handler{service: service, store: store, auth: auth, static: static, managementPort: strconv.Itoa(cfg.Ports.Range[0])}, nil
}

// LoginURL mints a one-time link for `dboss login`. It points at the console's loopback
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
	session, ok := h.auth.authenticate(w, r)
	if !ok {
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		h.serveAsset(w, r, "index.html")
	case r.Method == http.MethodGet && r.URL.Path == "/logs":
		h.writeLogs(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/assets/"):
		h.serveAsset(w, r, strings.TrimPrefix(r.URL.Path, "/assets/"))
	case r.Method == http.MethodGet && r.URL.Path == "/api/bootstrap":
		h.writeDashboard(w, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/apps":
		writeJSON(w, http.StatusOK, map[string]any{"apps": h.service.Apps(), "updated_at": time.Now().UTC()})
	case r.Method == http.MethodGet && r.URL.Path == "/api/logs":
		h.searchLogs(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/requests":
		h.searchRequests(w, r)
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
	if _, err := h.service.Do(ops.Request{Method: method, App: request.App, On: on}); err != nil {
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
	default:
		return "", false, errors.New("action must be start, stop, restart, maintenance-on, or maintenance-off")
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

func (h *Handler) writeLogs(w http.ResponseWriter, r *http.Request) {
	app := strings.TrimSpace(r.URL.Query().Get("app"))
	if app == "" {
		http.Error(w, "app is required", http.StatusBadRequest)
		return
	}
	logs, err := h.service.Logs(app, "", 1000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	names := slices.Sorted(maps.Keys(logs))
	var output strings.Builder
	for _, name := range names {
		if output.Len() > 0 {
			output.WriteByte('\n')
		}
		output.WriteString(strings.Join(logs[name], "\n"))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, output.String())
}

func (h *Handler) searchLogs(w http.ResponseWriter, r *http.Request) {
	app := strings.TrimSpace(r.URL.Query().Get("app"))
	if app == "" {
		writeError(w, http.StatusBadRequest, "app is required")
		return
	}
	entries, err := h.service.SearchLogs(app, logstore.LogFilter{
		Process: strings.TrimSpace(r.URL.Query().Get("process")),
		Level:   strings.TrimSpace(r.URL.Query().Get("level")),
		Query:   strings.TrimSpace(r.URL.Query().Get("q")),
		Since:   sinceParam(r.URL.Query().Get("since")),
		Limit:   intParam(r.URL.Query().Get("limit")),
	})
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": entries, "updated_at": time.Now().UTC()})
}

func (h *Handler) searchRequests(w http.ResponseWriter, r *http.Request) {
	app := strings.TrimSpace(r.URL.Query().Get("app"))
	if app == "" {
		writeError(w, http.StatusBadRequest, "app is required")
		return
	}
	entries, err := h.service.SearchRequests(app, logstore.RequestFilter{
		Status: intParam(r.URL.Query().Get("status")),
		Query:  strings.TrimSpace(r.URL.Query().Get("q")),
		Since:  sinceParam(r.URL.Query().Get("since")),
		Limit:  intParam(r.URL.Query().Get("limit")),
	})
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": entries, "updated_at": time.Now().UTC()})
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
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; connect-src 'self'; font-src 'self'; form-action 'self'; frame-ancestors 'none'; img-src 'none'; object-src 'none'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'")
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
