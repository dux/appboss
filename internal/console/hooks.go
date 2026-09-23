package console

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"time"

	"dboss/internal/httpx"
	"dboss/internal/logx"
	"dboss/internal/ops"
)

// handleHook accepts a signed ping at /hooks/<app>/<hook> and starts the hook. It is the one
// console route outside the session flow: the Git host cannot carry a session, so the hook's own
// secret is the credential. The request body is read raw for the GitHub HMAC.
func (h *Handler) handleHook(w http.ResponseWriter, r *http.Request) {
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
	secret, err := h.service.HookToken(app, hookName)
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
// deploys in the background, since a checkout can take minutes.
func (h *Handler) handleHostHook(w http.ResponseWriter, r *http.Request, name string, body []byte) {
	secret, err := h.service.HostHookToken(name)
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
// the query, Authorization header or X-Gitlab-Token. Every comparison is constant time, and an
// unset token refuses every ping.
func hookAuthorized(r *http.Request, secret string, body []byte) bool {
	if secret == "" {
		return false
	}
	if signature := r.Header.Get("X-Hub-Signature-256"); strings.HasPrefix(signature, "sha256=") {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		expected := hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(expected), []byte(strings.TrimPrefix(signature, "sha256=")))
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		token = httpx.BearerToken(r)
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
