package console

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"deploy-boss/internal/config"
	"deploy-boss/internal/reqlog"
	"deploy-boss/internal/super"
)

const maxRequestBody = 1 << 20

//go:embed static/*
var assets embed.FS

type AppManager interface {
	Snapshots() []super.Snapshot
	Start(string) error
	Stop(string) error
	Restart(string) error
	Rescan() ([]error, error)
}

type RateReader interface {
	Rates(string) (reqlog.Rates, error)
}

type Handler struct {
	manager AppManager
	rates   RateReader
	auth    *authenticator
	static  fs.FS
}

type dashboard struct {
	Viewer    string           `json:"viewer"`
	CSRF      string           `json:"csrf"`
	Apps      []super.Snapshot `json:"apps"`
	UpdatedAt time.Time        `json:"updated_at"`
}

type actionRequest struct {
	App    string `json:"app"`
	Action string `json:"action"`
}

type rescanResponse struct {
	Apps     []super.Snapshot `json:"apps"`
	Warnings []string         `json:"warnings"`
}

func New(cfg config.Config, manager AppManager, rates RateReader) (*Handler, error) {
	auth, err := newAuthenticator(cfg)
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	return &Handler{manager: manager, rates: rates, auth: auth, static: static}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if _, err := authDestination(r.Host, h.auth.host); err != nil {
		http.NotFound(w, r)
		return
	}
	session, ok := h.auth.authenticate(w, r)
	if !ok {
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		h.serveAsset(w, r, "index.html", "text/html; charset=utf-8")
	case r.Method == http.MethodGet && r.URL.Path == "/assets/app.css":
		h.serveAsset(w, r, "app.css", "text/css; charset=utf-8")
	case r.Method == http.MethodGet && r.URL.Path == "/assets/app.js":
		h.serveAsset(w, r, "app.js", "text/javascript; charset=utf-8")
	case r.Method == http.MethodGet && r.URL.Path == "/api/bootstrap":
		h.writeDashboard(w, session)
	case r.Method == http.MethodGet && r.URL.Path == "/api/apps":
		writeJSON(w, http.StatusOK, map[string]any{"apps": h.snapshots(), "updated_at": time.Now().UTC()})
	case r.Method == http.MethodPost && r.URL.Path == "/api/action":
		h.action(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/rescan":
		h.rescan(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/api/logout":
		h.logout(w, r, session)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request, path, contentType string) {
	data, err := fs.ReadFile(h.static, path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) writeDashboard(w http.ResponseWriter, session authSession) {
	writeJSON(w, http.StatusOK, dashboard{Viewer: session.Email, CSRF: session.CSRF, Apps: h.snapshots(), UpdatedAt: time.Now().UTC()})
}

func (h *Handler) snapshots() []super.Snapshot {
	snapshots := h.manager.Snapshots()
	if h.rates == nil {
		return snapshots
	}
	for index := range snapshots {
		rates, err := h.rates.Rates(snapshots[index].Name)
		if err != nil {
			continue
		}
		snapshots[index].RequestRates = super.RequestRates{LastMinute: rates.LastMinute, LastHour: rates.LastHour, LastDay: rates.LastDay}
	}
	return snapshots
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
	var err error
	switch request.Action {
	case "start":
		err = h.manager.Start(request.App)
	case "stop":
		err = h.manager.Stop(request.App)
	case "restart":
		err = h.manager.Restart(request.App)
	default:
		writeError(w, http.StatusBadRequest, "action must be start, stop, or restart")
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": h.snapshots(), "updated_at": time.Now().UTC()})
}

func (h *Handler) rescan(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	invalid, err := h.manager.Rescan()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	warnings := make([]string, 0, len(invalid))
	for _, invalidApp := range invalid {
		warnings = append(warnings, invalidApp.Error())
	}
	writeJSON(w, http.StatusOK, rescanResponse{Apps: h.snapshots(), Warnings: warnings})
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
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; connect-src 'self'; font-src 'self'; form-action 'self'; frame-ancestors 'none'; img-src 'none'; object-src 'none'; script-src 'self'; style-src 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
