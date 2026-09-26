package console

import (
	"cmp"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dboss/internal/logstore"
	"dboss/internal/ops"
)

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

// logBlocked lists the deny counters, shared by every app.
func (h *Handler) logBlocked(w http.ResponseWriter, r *http.Request) {
	rows, err := h.service.Blocked()
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "updated_at": time.Now().UTC()})
}

// logExceptions lists one app's aggregated exception groups for the Exceptions tab.
func (h *Handler) logExceptions(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	app := strings.TrimSpace(query.Get("app"))
	if app == "" {
		writeError(w, http.StatusBadRequest, "app is required")
		return
	}
	span, ok := trafficRanges[query.Get("range")]
	if !ok {
		writeError(w, http.StatusBadRequest, "range must be 1h, 24h, 7d or 30d")
		return
	}
	rows, err := h.service.Exceptions(app, logstore.ExceptionFilter{Since: time.Now().Add(-span)})
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": app, "rows": rows, "updated_at": time.Now().UTC()})
}

// exceptionResolve flips the is_resolved flag on one exception group. It mutates, so it goes
// through ops.Service.Do and leaves an audit row.
func (h *Handler) exceptionResolve(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		App    string `json:"app"`
		ExpUID string `json:"exp_uid"`
		On     bool   `json:"on"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	app := strings.TrimSpace(request.App)
	expUID := strings.TrimSpace(request.ExpUID)
	if app == "" || expUID == "" {
		writeError(w, http.StatusBadRequest, "app and exp_uid are required")
		return
	}
	if _, err := h.service.Do(ops.Request{Method: ops.ActionExceptionResolve, App: app, ExpUID: expUID, On: request.On, Actor: session.Email}); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated_at": time.Now().UTC()})
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

// idParam reads a row id out of the query. A missing or unparseable value is 0, which every
// filter reads as "not set".
func idParam(value string) int64 {
	parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
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
