package console

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"dboss/internal/apps"
	"dboss/internal/authcog"
	"dboss/internal/config"
	"dboss/internal/ops"
	"dboss/internal/supervisor"
	"dboss/internal/sysinfo"
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

// ConfigStore edits the config files dboss reads; apps.Store is the real one.
type ConfigStore interface {
	Files() ([]apps.ConfigFile, error)
	Read(id string) (apps.ConfigFile, error)
	Validate(id, contents string) error
	Write(id, contents, revision string) (apps.ConfigFile, error)
	CreateLocal(app string) (apps.ConfigFile, error)
	EnsureLocal(app string) (apps.ConfigFile, error)
	CreateHostLocal() (apps.ConfigFile, error)
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
	mux            *http.ServeMux
	sys            SysReader
	dev            bool
}

type dashboard struct {
	Viewer          string                `json:"viewer"`
	CSRF            string                `json:"csrf"`
	Apps            []supervisor.Snapshot `json:"apps"`
	RestartRequired []string              `json:"restart_required"`
	Capabilities    map[string]bool       `json:"capabilities"`
	Version         string                `json:"version"`
	UpdatedAt       time.Time             `json:"updated_at"`
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

type actionRequest struct {
	App    string `json:"app"`
	Action string `json:"action"`
	Job    string `json:"job,omitempty"`
}

func New(cfg config.Config, flow *authcog.Flow, service *ops.Service, store ConfigStore, sys SysReader) (*Handler, error) {
	auth, err := consoleAuthenticator(flow, cfg, service.HostPages)
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	// The console's own listener sits on the first port of the range, reserved by the allocator.
	handler := &Handler{service: service, store: store, auth: auth, static: static, managementPort: strconv.Itoa(cfg.Ports[0]), sys: sys, dev: cfg.Dev()}
	handler.mux = handler.routes()
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
	if _, err := authcog.Destination(rawHost, "https", h.auth.gate.Hosts); err == nil {
		return true
	}
	return loopbackHost(rawHost)
}
