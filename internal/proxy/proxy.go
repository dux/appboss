package proxy

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"app-boss/internal/config"
	"app-boss/internal/logstore"
	"app-boss/internal/super"
)

const maintenanceRetryAfter = 30

// Recorder receives one row per proxied request. logstore.Store is the production one.
type Recorder interface {
	Record(app string, retention time.Duration, entry logstore.RequestEntry) error
}

type Handler struct {
	cfg         config.Config
	manager     *super.Manager
	recorder    Recorder
	transport   *http.Transport
	starting    []byte
	crashed     []byte
	unknown     []byte
	maintenance []byte
	filters     []Filter
}

// New builds the proxy. extra stages are inserted before the forward stage, which is where a
// module hooks its own filter into the pipeline.
func New(cfg config.Config, manager *super.Manager, recorder Recorder, extra ...Filter) (*Handler, error) {
	starting, err := readPage(cfg.Proxy.Wake.StartingPage)
	if err != nil {
		return nil, err
	}
	crashed, err := readPage(cfg.Proxy.Wake.CrashedPage)
	if err != nil {
		return nil, err
	}
	unknown, err := readPage(cfg.Proxy.Wake.UnknownPage)
	if err != nil {
		return nil, err
	}
	maintenance, err := readPage("web/maintenance.html")
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: cfg.Proxy.Upstream.DialTimeout.Value()}).DialContext, ResponseHeaderTimeout: cfg.Proxy.Upstream.ResponseHeaderTimeout.Value(), IdleConnTimeout: cfg.Proxy.Upstream.IdleConnTimeout.Value(), MaxIdleConnsPerHost: cfg.Proxy.Upstream.MaxIdleConnsPerApp}
	h := &Handler{cfg: cfg, manager: manager, recorder: recorder, transport: transport, starting: starting, crashed: crashed, unknown: unknown, maintenance: maintenance}
	h.initFilters(extra...)
	return h, nil
}

// ServeHTTP resolves the app once and then walks the request through every proxy feature in a
// fixed order. Everything the steps need comes from the snapshot, so a rescan changes behaviour
// on the next request without any proxy state.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	snapshot, ok := h.manager.ResolveHost(r.Host)
	if !ok {
		h.page(w, http.StatusNotFound, h.unknown, "")
		return
	}
	started := time.Now()
	requestID := ensureRequestID(r)
	recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
	h.serve(recorder, r, snapshot)
	_ = h.recorder.Record(snapshot.Name, snapshot.LogRetention, logstore.RequestEntry{Time: started, Method: r.Method, Host: r.Host, Path: r.URL.RequestURI(), Status: recorder.status, DurationMS: time.Since(started).Milliseconds(), BytesOut: recorder.bytes, IP: clientIP(r, h.cfg.Proxy.ClientIPHeaders), UserAgent: r.UserAgent(), RequestID: requestID})
}

// redirectCanonical answers 301 to canonical_host for any other host the app owns, so www never
// serves content. The scheme follows X-Forwarded-Proto because TLS terminates at Cloudflare.
func redirectCanonical(w http.ResponseWriter, r *http.Request, canonical string) bool {
	if canonical == "" || strings.EqualFold(hostOnly(r.Host), canonical) {
		return false
	}
	scheme := "https"
	if proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); proto != "" {
		scheme = strings.ToLower(proto)
	}
	http.Redirect(w, r, scheme+"://"+canonical+r.URL.RequestURI(), http.StatusMovedPermanently)
	return true
}

func allowed(ip string, prefixes []netip.Prefix) bool {
	if len(prefixes) == 0 {
		return true
	}
	address, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	address = address.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// authorized enforces basic_auth. The user lookup is a plain map hit because the user list is
// not secret; the password comparison is bcrypt's own constant-time compare.
func authorized(w http.ResponseWriter, r *http.Request, snapshot super.Snapshot) bool {
	if len(snapshot.Web.BasicAuth) == 0 {
		return true
	}
	if user, password, ok := r.BasicAuth(); ok {
		if hash, found := snapshot.Web.BasicAuth[user]; found && bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil {
			return true
		}
	}
	w.Header().Set("WWW-Authenticate", fmt.Sprintf("Basic realm=%q", snapshot.Name))
	http.Error(w, "authentication required", http.StatusUnauthorized)
	return false
}

// serveStatic answers GET and HEAD for files under the static directory. Missing files and
// directories fall through to the app. The directory is resolved on every request so a release
// symlink swap is picked up immediately, and os.Root keeps the lookup inside it.
func serveStatic(w http.ResponseWriter, r *http.Request, snapshot super.Snapshot) bool {
	if snapshot.Web.Static == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	root, err := os.OpenRoot(resolveAppPath(snapshot.Dir, snapshot.Web.Static))
	if err != nil {
		return false
	}
	defer root.Close()
	cleaned := path.Clean("/" + r.URL.Path)
	if cleaned == "/" {
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

func cacheControl(requestPath string, immutable []string) string {
	for _, prefix := range immutable {
		if strings.HasPrefix(requestPath, prefix) {
			return "public, max-age=31536000, immutable"
		}
	}
	return "public, max-age=3600"
}

// limitRequest rejects declared oversize bodies before anything is read and caps chunked ones so
// the app never sees more than max_body bytes.
func limitRequest(w http.ResponseWriter, r *http.Request, limit int64) bool {
	if limit <= 0 {
		return true
	}
	if r.ContentLength > limit {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return false
	}
	if r.ContentLength < 0 && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}
	return true
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, snapshot super.Snapshot) {
	if snapshot.State != super.Running {
		if snapshot.State == super.Stopped {
			go func() {
				if err := h.manager.Start(snapshot.Name); err != nil {
					log.Printf("wake %s: %v", snapshot.Name, err)
				}
			}()
		}
		if snapshot.State == super.Crashed {
			h.unavailablePage(w, r, h.crashed, snapshot.Name, h.cfg.Proxy.Wake.RetryAfter)
			return
		}
		h.unavailablePage(w, r, h.starting, snapshot.Name, h.cfg.Proxy.Wake.RetryAfter)
		return
	}
	port := 0
	for _, process := range snapshot.Processes {
		if process.Name == snapshot.WebProcess {
			port = process.Port
			break
		}
	}
	if port == 0 {
		http.Error(w, "web process has no port", http.StatusBadGateway)
		return
	}
	h.manager.Touch(snapshot.Name)
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port)}
	reverse := httputil.NewSingleHostReverseProxy(target)
	reverse.Transport = h.transport
	reverse.ModifyResponse = func(response *http.Response) error {
		applyHeaders(response.Header, snapshot.Web.Headers)
		return nil
	}
	reverse.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	reverse.ServeHTTP(w, r)
	h.manager.Touch(snapshot.Name)
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

func (h *Handler) maintenancePage(snapshot super.Snapshot) []byte {
	var candidates []string
	if snapshot.Web.MaintenancePage != "" {
		candidates = append(candidates, resolveAppPath(snapshot.Dir, snapshot.Web.MaintenancePage))
	}
	if snapshot.Web.Static != "" {
		candidates = append(candidates, filepath.Join(resolveAppPath(snapshot.Dir, snapshot.Web.Static), "503.html"))
	}
	for _, candidate := range candidates {
		if data, err := os.ReadFile(candidate); err == nil {
			return data
		}
	}
	return h.maintenance
}

func resolveAppPath(dir, value string) string {
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(dir, value)
}

func (h *Handler) forbidden(w http.ResponseWriter, r *http.Request) {
	if wantsHTML(r) {
		h.page(w, http.StatusForbidden, []byte(defaultForbiddenPage), "")
		return
	}
	w.WriteHeader(http.StatusForbidden)
}

func (h *Handler) unavailablePage(w http.ResponseWriter, r *http.Request, page []byte, app string, retryAfter int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	if wantsHTML(r) {
		h.page(w, http.StatusServiceUnavailable, page, app)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

func wantsHTML(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (h *Handler) page(w http.ResponseWriter, status int, page []byte, app string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	contents := strings.ReplaceAll(string(page), "{{APP_NAME}}", app)
	contents = strings.ReplaceAll(contents, "{{RETRY_AFTER}}", strconv.Itoa(h.cfg.Proxy.Wake.RetryAfter))
	_, _ = w.Write([]byte(contents))
}

func readPage(path string) ([]byte, error) {
	if filepath.IsAbs(path) {
		return os.ReadFile(path)
	}
	if data, err := os.ReadFile(path); err == nil {
		return data, nil
	}
	executable, err := os.Executable()
	if err == nil {
		if data, readErr := os.ReadFile(filepath.Join(filepath.Dir(executable), path)); readErr == nil {
			return data, nil
		}
	}
	switch filepath.Base(path) {
	case "starting.html":
		return []byte(defaultStartingPage), nil
	case "crashed.html":
		return []byte(defaultCrashedPage), nil
	case "404.html":
		return []byte(defaultUnknownPage), nil
	case "maintenance.html":
		return []byte(defaultMaintenancePage), nil
	default:
		return nil, fmt.Errorf("read page %s: file not found", path)
	}
}

const pageStyle = `<style>body{background:#111;color:#eee;font:16px system-ui;display:grid;min-height:100vh;place-items:center;margin:0}main{text-align:center}p{color:#aaa}</style>`
const defaultStartingPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta http-equiv="refresh" content="{{RETRY_AFTER}}"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Starting {{APP_NAME}}</title>` + pageStyle + `<main><h1>Starting {{APP_NAME}}</h1><p>This page will refresh shortly.</p></main></html>`
const defaultCrashedPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{APP_NAME}} is unavailable</title>` + pageStyle + `<main><h1>{{APP_NAME}} is unavailable</h1><p>The application could not be started.</p></main></html>`
const defaultUnknownPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Not found</title>` + pageStyle + `<main><h1>Application not found</h1></main></html>`
const defaultMaintenancePage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{APP_NAME}} is down for maintenance</title>` + pageStyle + `<main><h1>Down for maintenance</h1><p>{{APP_NAME}} will be back shortly.</p></main></html>`
const defaultForbiddenPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Forbidden</title>` + pageStyle + `<main><h1>Forbidden</h1><p>Your address is not allowed to reach this application.</p></main></html>`

func clientIP(r *http.Request, headers []string) string {
	for _, header := range headers {
		if value := r.Header.Get(header); value != "" {
			return strings.TrimSpace(strings.Split(value, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

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
