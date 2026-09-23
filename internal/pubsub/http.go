package pubsub

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"dboss/internal/config"
	"dboss/internal/httpx"
	"dboss/internal/supervisor"
)

// Reserved path segments that never name a user channel.
const (
	clientPath      = "client.js"
	testPath        = "_test"
	testPublishPath = "_test/publish"
	testChannel     = "_selftest"
)

// channelPattern is the accepted channel name: one URL path segment, no reserved names.
var channelPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,64}$`)

// Filter is the proxy stage. It answers the pubsub path of the web process that owns the request
// host and lets every other request through.
func (s *Service) Filter(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	web, ok := app.WebForHost(r.Host)
	if !ok || !web.Pubsub.Enabled() {
		next()
		return
	}
	cfg := web.Pubsub
	rest, ok := route(cfg.Path, r.URL.Path)
	if !ok {
		next()
		return
	}
	switch {
	case rest == "":
		next()
	case rest == clientPath:
		s.serveClient(w, r)
	case rest == testPath:
		if !cfg.Test {
			next()
			return
		}
		s.serveTestPage(w, r, cfg)
	case rest == testPublishPath:
		if !cfg.Test {
			next()
			return
		}
		s.serveTestPublish(w, r, app.Name, web)
	case rest == testChannel:
		if !cfg.Test {
			next()
			return
		}
		s.serveChannel(w, r, app.Name, web, rest)
	case ValidChannel(rest):
		s.serveChannel(w, r, app.Name, web, rest)
	default:
		next()
	}
}

// AuthorizesPublish reports whether the request is a publish to this app carrying a valid secret.
// The proxy's basic-auth stage consults it so a publisher needs only the publish secret, even when
// the app also has basic_auth.
func (s *Service) AuthorizesPublish(r *http.Request, app supervisor.Snapshot) bool {
	web, ok := app.WebForHost(r.Host)
	if !ok || !web.Pubsub.Enabled() || r.Method != http.MethodPost {
		return false
	}
	rest, ok := route(web.Pubsub.Path, r.URL.Path)
	if !ok || !ValidChannel(rest) {
		return false
	}
	return s.authorize(r, app.Name, web)
}

func (s *Service) serveChannel(w http.ResponseWriter, r *http.Request, app string, web supervisor.WebProcessSnapshot, channel string) {
	switch r.Method {
	case http.MethodPost:
		s.servePublish(w, r, app, web, channel)
	case http.MethodGet, http.MethodHead:
		if upgradeRequested(r) {
			s.serveWebSocket(w, r, app, web, channel)
			return
		}
		if acceptsEventStream(r) {
			s.serveSSE(w, r, app, web, channel)
			return
		}
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "connect with a websocket upgrade or Accept: text/event-stream", http.StatusUpgradeRequired)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Service) servePublish(w http.ResponseWriter, r *http.Request, app string, web supervisor.WebProcessSnapshot, channel string) {
	if !s.authorize(r, app, web) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="pubsub"`)
		http.Error(w, "invalid or missing publish secret", http.StatusUnauthorized)
		return
	}
	cfg := web.Pubsub
	body, err := readBody(r, cfg.MaxMessageSize)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	delivered := s.publish(hubID{app, web.Name}, channel, parseMessage(body), cfg.Replay)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"channel": channel, "subscribers": delivered})
}

func (s *Service) serveClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(s.client)
}

func (s *Service) serveTestPublish(w http.ResponseWriter, r *http.Request, app string, web supervisor.WebProcessSnapshot) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Nonce string `json:"nonce"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&request)
	data, _ := json.Marshal(map[string]string{"nonce": request.Nonce})
	s.publish(hubID{app, web.Name}, testChannel, Message{Event: "selftest", Data: data}, 0)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"ok":true}`+"\n")
}

// authorize compares the request's publish token against the hub's effective secret in constant
// time.
func (s *Service) authorize(r *http.Request, app string, web supervisor.WebProcessSnapshot) bool {
	secret, err := s.Secret(app, web.Name, web.Pubsub)
	if err != nil || secret == "" {
		return false
	}
	presented := publishToken(r)
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) == 1
}

// route returns the path after the mount prefix. The bare prefix yields "", a path outside the
// prefix yields ok=false.
func route(prefix, requestPath string) (string, bool) {
	if requestPath == prefix {
		return "", true
	}
	if strings.HasPrefix(requestPath, prefix+"/") {
		return strings.TrimPrefix(requestPath, prefix+"/"), true
	}
	return "", false
}

// ValidChannel reports whether name can be a channel: the reserved segments never are.
func ValidChannel(name string) bool {
	if name == testChannel || name == clientPath || name == testPath {
		return false
	}
	return channelPattern.MatchString(name)
}

func upgradeRequested(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

func acceptsEventStream(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// publishToken reads the secret from the query, the bearer header or X-Pubsub-Token, the same
// ways a deploy hook accepts its secret.
func publishToken(r *http.Request) string {
	if token := r.URL.Query().Get("token"); token != "" {
		return token
	}
	if token := httpx.BearerToken(r); token != "" {
		return token
	}
	return r.Header.Get("X-Pubsub-Token")
}

func readBody(r *http.Request, limit config.Size) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	if limit > 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
		if err != nil {
			return nil, err
		}
		if int64(len(body)) > int64(limit) {
			return nil, errors.New("message too large")
		}
		return body, nil
	}
	return io.ReadAll(r.Body)
}

// parseMessage accepts a {"event","data"} envelope or any body, which becomes the data of a
// "message" event.
func parseMessage(body []byte) Message {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return Message{Event: "message", Data: json.RawMessage("null")}
	}
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		data, _ := json.Marshal(string(body))
		return Message{Event: "message", Data: data}
	}
	if object, ok := value.(map[string]any); ok {
		event, _ := object["event"].(string)
		data, hasData := object["data"]
		if hasData || event != "" {
			raw, err := json.Marshal(data)
			if err != nil {
				raw = json.RawMessage("null")
			}
			if event == "" {
				event = "message"
			}
			return Message{Event: event, Data: raw}
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		raw = json.RawMessage("null")
	}
	return Message{Event: "message", Data: raw}
}

// parseClientMessage accepts a client event from a WebSocket: an object with a non-empty event.
func parseClientMessage(data []byte) (Message, bool) {
	var envelope struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Event == "" {
		return Message{}, false
	}
	if len(envelope.Data) == 0 {
		envelope.Data = json.RawMessage("null")
	}
	return Message{Event: envelope.Event, Data: envelope.Data}, true
}

// recordSuppressor is implemented by the proxy's response recorder. A long-lived subscribe request
// suppresses its request-log row so a multi-hour socket cannot skew latency.
type recordSuppressor interface{ SuppressRecord() }

func suppressRecord(w http.ResponseWriter) {
	if recorder, ok := w.(recordSuppressor); ok {
		recorder.SuppressRecord()
	}
}
