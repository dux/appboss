package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"dboss/internal/ops"
	"dboss/internal/pubsub"
)

// writePubsub returns the realtime tab: every app that serves channels plus the integration guide.
func (h *Handler) writePubsub(w http.ResponseWriter, _ *http.Request) {
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
	info, err := h.service.Do(ops.Request{Method: ops.ActionPubsubRotate, App: strings.TrimSpace(request.App), Process: strings.TrimSpace(request.Process), Actor: session.Email})
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
