package proxy

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"deploy-boss/internal/config"
	"deploy-boss/internal/reqlog"
	"deploy-boss/internal/super"
)

type Handler struct {
	cfg       config.Config
	manager   *super.Manager
	logs      *reqlog.Manager
	transport *http.Transport
	starting  []byte
	crashed   []byte
	unknown   []byte
}

func New(cfg config.Config, manager *super.Manager, logs *reqlog.Manager) (*Handler, error) {
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
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: cfg.Proxy.Upstream.DialTimeout.Value()}).DialContext, ResponseHeaderTimeout: cfg.Proxy.Upstream.ResponseHeaderTimeout.Value(), IdleConnTimeout: cfg.Proxy.Upstream.IdleConnTimeout.Value(), MaxIdleConnsPerHost: cfg.Proxy.Upstream.MaxIdleConnsPerApp}
	return &Handler{cfg: cfg, manager: manager, logs: logs, transport: transport, starting: starting, crashed: crashed, unknown: unknown}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	snapshot, ok := h.manager.ResolveHost(r.Host)
	if !ok {
		h.page(w, http.StatusNotFound, h.unknown, "")
		return
	}
	if snapshot.State != super.Running {
		if snapshot.State == super.Stopped {
			go func() {
				if err := h.manager.Start(snapshot.Name); err != nil {
					log.Printf("wake %s: %v", snapshot.Name, err)
				}
			}()
		}
		if snapshot.State == super.Crashed {
			h.unavailable(w, r, h.crashed, snapshot.Name)
			return
		}
		h.unavailable(w, r, h.starting, snapshot.Name)
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
	started := time.Now()
	recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port)}
	reverse := httputil.NewSingleHostReverseProxy(target)
	reverse.Transport = h.transport
	reverse.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	reverse.ServeHTTP(recorder, r)
	h.manager.Touch(snapshot.Name)
	_ = h.logs.Record(snapshot.Name, snapshot.LogRetention, snapshot.LogFlush, reqlog.Entry{Time: started, Method: r.Method, Host: r.Host, Path: r.URL.RequestURI(), Status: recorder.status, DurationMS: time.Since(started).Milliseconds(), BytesOut: recorder.bytes, IP: clientIP(r, h.cfg.Proxy.ClientIPHeaders), UserAgent: r.UserAgent()})
}

func (h *Handler) unavailable(w http.ResponseWriter, r *http.Request, page []byte, app string) {
	w.Header().Set("Retry-After", strconv.Itoa(h.cfg.Proxy.Wake.RetryAfter))
	if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
		h.page(w, http.StatusServiceUnavailable, page, app)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
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
	default:
		return nil, fmt.Errorf("read page %s: file not found", path)
	}
}

const defaultStartingPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta http-equiv="refresh" content="{{RETRY_AFTER}}"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Starting {{APP_NAME}}</title><style>body{background:#111;color:#eee;font:16px system-ui;display:grid;min-height:100vh;place-items:center;margin:0}main{text-align:center}p{color:#aaa}</style><main><h1>Starting {{APP_NAME}}</h1><p>This page will refresh shortly.</p></main></html>`
const defaultCrashedPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{APP_NAME}} is unavailable</title><style>body{background:#111;color:#eee;font:16px system-ui;display:grid;min-height:100vh;place-items:center;margin:0}main{text-align:center}p{color:#aaa}</style><main><h1>{{APP_NAME}} is unavailable</h1><p>The application could not be started.</p></main></html>`
const defaultUnknownPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Not found</title><style>body{background:#111;color:#eee;font:16px system-ui;display:grid;min-height:100vh;place-items:center;margin:0}</style><main><h1>Application not found</h1></main></html>`

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
