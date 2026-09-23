package console

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"dboss/internal/logstore"
	"dboss/internal/ops"
)

// audit answers the console Audit tab and the CLI with the newest operator actions.
func (h *Handler) audit(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	rows, err := h.service.SearchAudit(logstore.AuditFilter{
		ID:     idParam(query.Get("row")),
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
func (h *Handler) writeSys(w http.ResponseWriter, _ *http.Request) {
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
	result, err := h.service.Do(ops.Request{Method: ops.ActionRescan, Actor: session.Email})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
