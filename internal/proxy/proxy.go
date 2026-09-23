package proxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"dboss/internal/authcog"
	"dboss/internal/config"
	"dboss/internal/logstore"
	"dboss/internal/pages"
	"dboss/internal/supervisor"
)

// Retry-After of the maintenance page and of the pages in front of a starting app; the starting
// page also reloads itself after wakeRetryAfter.
const (
	maintenanceRetryAfter = 30
	wakeRetryAfter        = 5
)

// Upstream connection pool to the apps; the response timeout is proxy.timeout.
const (
	upstreamDialTimeout     = 2 * time.Second
	upstreamIdleTimeout     = 90 * time.Second
	upstreamIdleConnsPerApp = 32
)

// Recorder receives one row per proxied request. logstore.Store is the production one.
type Recorder interface {
	Record(app string, retention time.Duration, entry logstore.RequestEntry) error
}

// PublishAuthorizer lets a module vouch for a request that basic_auth would otherwise reject, so
// a pubsub publisher needs only its own publish secret. A nil authorizer disables the check.
type PublishAuthorizer interface {
	AuthorizesPublish(r *http.Request, app supervisor.Snapshot) bool
}

type Handler struct {
	cfg       config.Config
	manager   *supervisor.Manager
	recorder  Recorder
	pubsub    PublishAuthorizer
	signin    *authcog.Flow
	transport *http.Transport
	// hostConfig is the live host config, for the keys a rescan may change (pages, realm).
	hostConfig func() config.Config
	filters    []Filter
}

// New builds the proxy. extra stages are inserted before the forward stage, which is where a
// module hooks its own filter into the pipeline.
func New(cfg config.Config, signin *authcog.Flow, manager *supervisor.Manager, recorder Recorder, authorizer PublishAuthorizer, extra ...Filter) (*Handler, error) {
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: upstreamDialTimeout}).DialContext, ResponseHeaderTimeout: cfg.Proxy.Timeout.Value(), IdleConnTimeout: upstreamIdleTimeout, MaxIdleConnsPerHost: upstreamIdleConnsPerApp}
	h := &Handler{cfg: cfg, manager: manager, recorder: recorder, pubsub: authorizer, signin: signin, transport: transport, hostConfig: manager.HostConfig}
	h.initFilters(extra...)
	return h, nil
}

// ServeHTTP resolves the app once and then walks the request through every proxy feature in a
// fixed order. Everything the steps need comes from the snapshot, so a rescan changes behaviour
// on the next request without any proxy state.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == pages.LogoPath && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		pages.ServeLogo(w)
		return
	}
	snapshot, ok := h.manager.ResolveHost(r.Host)
	if !ok {
		pages.Page{Name: pages.NotFound}.Write(w, h.hostPages())
		return
	}
	started := time.Now()
	requestID := ensureRequestID(r)
	recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
	h.serve(recorder, r, snapshot)
	if recorder.suppressed {
		return
	}
	process := ""
	if web, ok := snapshot.WebForHost(r.Host); ok {
		process = web.Name
	}
	_ = h.recorder.Record(snapshot.Name, snapshot.LogRetention, logstore.RequestEntry{Time: started, Method: r.Method, Host: r.Host, Path: r.URL.RequestURI(), Status: recorder.status, DurationMS: time.Since(started).Milliseconds(), BytesOut: recorder.bytes, IP: clientIP(r, h.cfg.Proxy.Cloudflare), UserAgent: r.UserAgent(), RequestID: requestID, Process: process, Country: country(r)})
}

// redirectCanonical answers 301 to canonical_host for any other host the app owns, so www never
// serves content. The scheme is the one the request arrived on: Cloudflare's X-Forwarded-Proto,
// else dboss's own TLS, else plain HTTP. Assuming https here sent a plain-HTTP origin, and every
// local session, to a port nothing listens on.
func redirectCanonical(w http.ResponseWriter, r *http.Request, canonical string) bool {
	if canonical == "" || config.NormalizeHost(r.Host) == config.NormalizePattern(canonical) {
		return false
	}
	http.Redirect(w, r, requestScheme(r)+"://"+canonical+r.URL.RequestURI(), http.StatusMovedPermanently)
	return true
}

func allowed(ip string, prefixes []netip.Prefix) bool {
	return len(prefixes) == 0 || inPrefixes(ip, prefixes)
}

// authorized checks basic_auth. The user lookup is a plain map hit because the user list is not
// secret. A value bcrypt can parse is a hash and goes through bcrypt's compare; anything else is a
// plain password, compared in constant time over sha256 digests so its length does not leak.
func authorized(r *http.Request, snapshot supervisor.Snapshot) bool {
	if len(snapshot.Web.BasicAuth) == 0 {
		return true
	}
	user, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	want, found := snapshot.Web.BasicAuth[user]
	if !found {
		return false
	}
	if _, err := bcrypt.Cost([]byte(want)); err == nil {
		return bcrypt.CompareHashAndPassword([]byte(want), []byte(password)) == nil
	}
	got, expected := sha256.Sum256([]byte(password)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(got[:], expected[:]) == 1
}

// serveStatic answers GET and HEAD for files under the static directory. Missing files and
// directories fall through to the app. The directory is resolved on every request so a release
// symlink swap is picked up immediately, and os.Root keeps the lookup inside it.
func serveStatic(w http.ResponseWriter, r *http.Request, snapshot supervisor.Snapshot) bool {
	web, ok := snapshot.WebForHost(r.Host)
	if !ok || web.Static == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	root, err := os.OpenRoot(resolveAppPath(snapshot.Dir, web.Static))
	if err != nil {
		return false
	}
	defer root.Close()
	cleaned := path.Clean("/" + r.URL.Path)
	if cleaned == "/" || !staticExtension(cleaned, snapshot.Web.StaticExtensions) {
		return false
	}
	file, err := root.Open(strings.TrimPrefix(cleaned, "/"))
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		return false
	}
	w.Header().Set("Cache-Control", cacheControl(cleaned, snapshot.Web.StaticImmutable))
	applyHeaders(w.Header(), snapshot.Web.Headers)
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	return true
}

// staticExtension keeps static serving to the asset types in static_extensions, so an HTML page,
// a dotfile or an extensionless path under the directory is always the app's to answer. An empty
// list serves any file.
func staticExtension(requestPath string, extensions []string) bool {
	if len(extensions) == 0 {
		return true
	}
	extension := strings.ToLower(strings.TrimPrefix(path.Ext(requestPath), "."))
	return extension != "" && slices.Contains(extensions, extension)
}

func cacheControl(requestPath string, immutable []string) string {
	for _, prefix := range immutable {
		if strings.HasPrefix(requestPath, prefix) {
			return "public, max-age=31536000, immutable"
		}
	}
	return "public, max-age=3600"
}

const bodyMemoryLimit = 1 << 20

// bufferRequest reads the whole request body before the app is contacted, so the app never sees a
// partial upload when a client is slow or disconnects. Bodies up to bodyMemoryLimit stay in memory,
// larger ones spill to a temp file that is removed once the request is done. A body over max_body
// is rejected with 413 without touching the app; max_body 0 means unlimited.
func bufferRequest(w http.ResponseWriter, r *http.Request, limit int64, next func()) {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		next()
		return
	}
	if limit > 0 && r.ContentLength > limit {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	source := io.Reader(r.Body)
	if limit > 0 {
		source = http.MaxBytesReader(w, r.Body, limit)
	}
	spill := &spillWriter{}
	n, err := io.Copy(spill, source)
	if err != nil {
		spill.close()
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}
	body, err := spill.reader()
	if err != nil {
		spill.close()
		http.Error(w, "could not buffer request body", http.StatusInternalServerError)
		return
	}
	r.Body = body
	r.ContentLength = n
	r.TransferEncoding = nil
	r.Header.Del("Expect")
	defer spill.close()
	next()
}

// spillWriter buffers the first bodyMemoryLimit bytes in memory and switches to a 0600 temp file
// beyond that, so a large upload never grows the heap without bound.
type spillWriter struct {
	mem  bytes.Buffer
	file *os.File
}

func (s *spillWriter) Write(data []byte) (int, error) {
	if s.file == nil && s.mem.Len()+len(data) > bodyMemoryLimit {
		file, err := os.CreateTemp("", "dboss-body-*")
		if err != nil {
			return 0, err
		}
		if _, err := file.Write(s.mem.Bytes()); err != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
			return 0, err
		}
		s.mem.Reset()
		s.file = file
	}
	if s.file != nil {
		return s.file.Write(data)
	}
	return s.mem.Write(data)
}

func (s *spillWriter) reader() (io.ReadCloser, error) {
	if s.file == nil {
		return io.NopCloser(bytes.NewReader(s.mem.Bytes())), nil
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return s.file, nil
}

func (s *spillWriter) close() {
	if s.file == nil {
		return
	}
	name := s.file.Name()
	_ = s.file.Close()
	_ = os.Remove(name)
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, snapshot supervisor.Snapshot) {
	// A draining app is stopping or restarting: answer new requests now, let in-flight ones run.
	if snapshot.Draining {
		h.unavailablePage(w, r, snapshot, pages.Starting, wakeRetryAfter)
		return
	}
	if snapshot.State != supervisor.Running {
		if snapshot.State == supervisor.Stopped && h.manager != nil && !h.manager.Booted() {
			// Wake is a no-op until the session boots, so say what it waits for.
			h.unavailablePage(w, r, snapshot, pages.Waiting, wakeRetryAfter)
			return
		}
		if snapshot.State == supervisor.Stopped {
			// A button app only wakes on the deliberate POST of its start page, so a GET for
			// a favicon or a crawler never starts it.
			if snapshot.WakeButton && r.Method != http.MethodPost {
				h.stoppedPage(w, r, snapshot)
				return
			}
			go h.manager.Wake(snapshot.Name)
			if snapshot.WakeButton {
				// The POST came from our own page: answer the starting page directly instead
				// of an empty 503, and let its refresh follow the app up.
				w.Header().Set("Retry-After", strconv.Itoa(wakeRetryAfter))
				w.Header().Set("Refresh", strconv.Itoa(wakeRetryAfter))
				pages.Page{Name: pages.Starting, App: snapshot.Name}.Write(w, h.pageDirs(snapshot)...)
				return
			}
		}
		if snapshot.State == supervisor.Crashed {
			h.unavailablePage(w, r, snapshot, pages.Crashed, wakeRetryAfter)
			return
		}
		h.unavailablePage(w, r, snapshot, pages.Starting, wakeRetryAfter)
		return
	}
	web, ok := snapshot.WebForHost(r.Host)
	if !ok {
		h.errorPage(w, r, snapshot, http.StatusBadGateway)
		return
	}
	port, done, ok := h.manager.Pick(snapshot.Name, web.Name)
	if !ok {
		h.errorPage(w, r, snapshot, http.StatusBadGateway)
		return
	}
	defer done()
	h.manager.Touch(snapshot.Name)
	h.applyForwardedHeaders(r)
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port)}
	reverse := httputil.NewSingleHostReverseProxy(target)
	reverse.Transport = h.transport
	reverse.ModifyResponse = func(response *http.Response) error {
		applyHeaders(response.Header, snapshot.Web.Headers)
		replaceAppError(response, r, snapshot)
		return nil
	}
	reverse.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		h.errorPage(w, r, snapshot, http.StatusBadGateway)
	}
	reverse.ServeHTTP(w, r)
	h.manager.Touch(snapshot.Name)
}

// errorPage answers a failure of the proxy itself: an HTML GET gets the error page, anything else
// the bare status.
func (h *Handler) errorPage(w http.ResponseWriter, r *http.Request, snapshot supervisor.Snapshot, status int) {
	w.Header().Set("Cache-Control", "no-store")
	if !wantsHTML(r) {
		w.WriteHeader(status)
		return
	}
	pages.Page{Name: pages.Error, App: snapshot.Name, Status: status}.Write(w, h.pageDirs(snapshot)...)
}

// replaceAppError swaps the body of an app 5xx answer to an HTML GET for the error page and keeps
// the status. It only acts when the app's own pages folder has error.html or template.html, so an
// app that ships no pages keeps its own error bodies and API answers are never rewritten.
func replaceAppError(response *http.Response, r *http.Request, snapshot supervisor.Snapshot) {
	if response.StatusCode < http.StatusInternalServerError || !wantsHTML(r) {
		return
	}
	_, page := pages.Lookup(pages.Error, snapshot.Pages)
	if page == nil {
		return
	}
	data := pages.Page{Name: pages.Error, App: snapshot.Name, Status: response.StatusCode}.Fill(page)
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(data))
	response.ContentLength = int64(len(data))
	for _, name := range []string{"Content-Encoding", "Etag", "Last-Modified", "Transfer-Encoding"} {
		response.Header.Del(name)
	}
	response.TransferEncoding = nil
	response.Header.Set("Content-Length", strconv.Itoa(len(data)))
	response.Header.Set("Content-Type", "text/html; charset=utf-8")
	response.Header.Set("Cache-Control", "no-store")
}

// applyForwardedHeaders adds the headers an app expects from a reverse proxy, but never
// overrides what Cloudflare already sent.
func (h *Handler) applyForwardedHeaders(r *http.Request) {
	if r.Header.Get("X-Forwarded-Proto") == "" {
		r.Header.Set("X-Forwarded-Proto", requestScheme(r))
	}
	if r.Header.Get("X-Forwarded-Host") == "" {
		r.Header.Set("X-Forwarded-Host", r.Host)
	}
	if r.Header.Get("X-Real-IP") == "" {
		if ip := clientIP(r, h.cfg.Proxy.Cloudflare); ip != "" {
			r.Header.Set("X-Real-IP", ip)
		}
	}
}

// requestScheme is the public scheme of the request: X-Forwarded-Proto when Cloudflare set it,
// else TLS, else plain HTTP.
func requestScheme(r *http.Request) string {
	if proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); proto != "" {
		return strings.ToLower(proto)
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// applyHeaders sets the configured response headers; an empty value removes the header instead.
func applyHeaders(header http.Header, values map[string]string) {
	for name, value := range values {
		if value == "" {
			header.Del(name)
		} else {
			header.Set(name, value)
		}
	}
}

// ensureRequestID forwards CF-Ray as X-Request-ID, or mints one, so app logs and the request
// log can be joined on the same value.
func ensureRequestID(r *http.Request) string {
	id := r.Header.Get("CF-Ray")
	if id == "" {
		var raw [16]byte
		_, _ = rand.Read(raw[:])
		id = hex.EncodeToString(raw[:])
	}
	r.Header.Set("X-Request-ID", id)
	return id
}

// hostPages is the host's pages folder from the live host config, so a rescan applies it.
func (h *Handler) hostPages() string { return h.hostConfig().Pages }

// pageDirs is the lookup order of an app's pages: its own folder, then the host's.
func (h *Handler) pageDirs(app supervisor.Snapshot) []string {
	return []string{app.Pages, h.hostPages()}
}

func (h *Handler) forbidden(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot) {
	if wantsHTML(r) {
		pages.Page{Name: pages.Forbidden, App: app.Name}.Write(w, h.pageDirs(app)...)
		return
	}
	w.WriteHeader(http.StatusForbidden)
}

// unavailablePage answers 503 with Retry-After; an HTML GET gets the page, and the starting and
// waiting pages also reload themselves after the same delay.
func (h *Handler) unavailablePage(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, name pages.Name, retryAfter int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	if !wantsHTML(r) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if name == pages.Starting || name == pages.Waiting {
		w.Header().Set("Refresh", strconv.Itoa(retryAfter))
	}
	pages.Page{Name: name, App: app.Name}.Write(w, h.pageDirs(app)...)
}

// stoppedPage is the answer for a stopped button app on anything but POST: a page with a start
// button whose form posts back to the same URL. It carries no Retry-After and no reload so
// nothing but a deliberate click starts the app.
func (h *Handler) stoppedPage(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot) {
	w.Header().Set("Cache-Control", "no-store")
	if !wantsHTML(r) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	action := `<form method="post"><button type="submit">Start ` + html.EscapeString(app.Name) + `</button></form>`
	pages.Page{Name: pages.Stopped, App: app.Name, Action: action}.Write(w, h.pageDirs(app)...)
}

func wantsHTML(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html")
}

// clientIP is the visitor's address: CF-Connecting-IP behind Cloudflare, where only Cloudflare
// can connect and the header cannot be spoofed, else the connection's own address.
func clientIP(r *http.Request, cloudflare bool) string {
	if value := r.Header.Get("CF-Connecting-IP"); cloudflare && value != "" {
		return strings.TrimSpace(value)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// country is the visitor country Cloudflare reports in CF-IPCountry. Only a two-character code is
// kept (T1 is Tor), so a missing or junk header never reaches the request log.
func country(r *http.Request) string {
	code := strings.ToUpper(strings.TrimSpace(r.Header.Get("CF-IPCountry")))
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || (code[1] < 'A' || code[1] > 'Z') && (code[1] < '0' || code[1] > '9') {
		return ""
	}
	return code
}

type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
	suppressed  bool
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// SuppressRecord keeps a long-lived response out of the request log. The pubsub filter calls it
// once a WebSocket or SSE stream is established, so its lifetime cannot skew request latency.
func (r *responseRecorder) SuppressRecord() { r.suppressed = true }

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
func (r *responseRecorder) Write(data []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(data)
	r.bytes += int64(n)
	return n, err
}
func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("hijacking is not supported")
	}
	return hijacker.Hijack()
}
func (r *responseRecorder) ReadFrom(source io.Reader) (int64, error) {
	if reader, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err := reader.ReadFrom(source)
		r.bytes += n
		return n, err
	}
	n, err := io.Copy(struct{ io.Writer }{r.ResponseWriter}, source)
	r.bytes += n
	return n, err
}

func resolveAppPath(dir, value string) string {
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(dir, value)
}
