package console

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dboss/internal/ops"
)

// The Events tab reads through the ops service like the CLI. Reads are GETs without an audit
// row; SQL and saving or deleting a view go through Do and are audited. A funnel run is a POST
// because the unsaved builder sends its definition, but it only reads.

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	summary, err := h.service.EventSummary(strings.TrimSpace(query.Get("app")), query.Get("filter"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"summary": summary, "updated_at": time.Now().UTC()})
}

func (h *Handler) eventsLatest(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, _ := strconv.Atoi(query.Get("limit"))
	rows, err := h.service.LatestEvents(strings.TrimSpace(query.Get("app")), query.Get("filter"), limit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": rows, "updated_at": time.Now().UTC()})
}

func (h *Handler) eventsFacets(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	facets, err := h.service.EventFacets(strings.TrimSpace(query.Get("app")), query.Get("filter"), query.Get("key"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"facets": facets, "updated_at": time.Now().UTC()})
}

func (h *Handler) eventsViews(w http.ResponseWriter, r *http.Request) {
	result, err := h.service.EventViews(strings.TrimSpace(r.URL.Query().Get("app")))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"views": result, "updated_at": time.Now().UTC()})
}

func (h *Handler) eventsFunnel(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		App    string          `json:"app"`
		Name   string          `json:"name"`
		Funnel json.RawMessage `json:"funnel"`
		Filter string          `json:"filter"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.service.RunFunnel(strings.TrimSpace(request.App), request.Name, request.Funnel, request.Filter)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result, "updated_at": time.Now().UTC()})
}

func (h *Handler) eventsQuery(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		App string `json:"app"`
		SQL string `json:"sql"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.service.Do(ops.Request{Method: ops.ActionEventsQuery, App: strings.TrimSpace(request.App), SQL: request.SQL, Actor: session.Email})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result, "updated_at": time.Now().UTC()})
}

// eventsSave stores a view ({kind: view, entry: {name, filter}}) or a funnel from the console.
func (h *Handler) eventsSave(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		App   string          `json:"app"`
		Kind  string          `json:"kind"`
		Entry json.RawMessage `json:"entry"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := h.service.Do(ops.Request{Method: ops.ActionEventsSave, App: strings.TrimSpace(request.App), Kind: request.Kind, Data: request.Entry, Actor: session.Email}); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	result, err := h.service.EventViews(strings.TrimSpace(request.App))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"views": result, "updated_at": time.Now().UTC()})
}

func (h *Handler) eventsDelete(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		App  string `json:"app"`
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := h.service.Do(ops.Request{Method: ops.ActionEventsDelete, App: strings.TrimSpace(request.App), Kind: request.Kind, Name: request.Name, Actor: session.Email}); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": request.Name, "updated_at": time.Now().UTC()})
}
