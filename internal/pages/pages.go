// Package pages renders every HTML page dboss answers with itself: the wake, maintenance and
// error pages in front of an app and the host pages. Each page is looked up as a file first, so
// an app or the host can replace any of them, and falls back to the built-in template.
package pages

import (
	"embed"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Name identifies one page; the file that replaces it is <name>.html.
type Name string

const (
	Starting    Name = "starting"
	Waiting     Name = "waiting"
	Stopped     Name = "stopped"
	Crashed     Name = "crashed"
	Maintenance Name = "maintenance"
	Error       Name = "error"
	Forbidden   Name = "forbidden"
	SignedOut   Name = "signed_out"
	NotFound    Name = "404"
	Login       Name = "login"
)

// TemplateFile is the one file that renders every page a folder does not name on its own.
const TemplateFile = "template.html"

// LogoPath is where the proxy and the console serve Logo, on every host.
const LogoPath = "/.well-known/dboss/logo.svg"

// Spec is one page: its default status and wording. %s in Title and Message is the app name.
// A host page is served without an app, so only the host folder can replace it.
type Spec struct {
	Name    Name   `json:"name"`
	Status  int    `json:"status"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Host    bool   `json:"host"`
	When    string `json:"when"`
}

// Specs lists every page in the order `dboss pages` prints them.
var Specs = []Spec{
	{Name: Starting, Status: http.StatusServiceUnavailable, Title: "%s is starting", Message: "It will be ready in a few seconds. This page reloads on its own.", When: "the app is waking up"},
	{Name: Waiting, Status: http.StatusServiceUnavailable, Title: "%s is waiting to start", Message: "dboss starts the apps once ENTER is pressed in the terminal that ran dboss start. This page reloads on its own.", When: "a hand-run session waits for ENTER"},
	{Name: Stopped, Status: http.StatusServiceUnavailable, Title: "%s is stopped", Message: "Start it to continue.", When: "a start-by-button app is stopped"},
	{Name: Crashed, Status: http.StatusServiceUnavailable, Title: "%s is not running", Message: "It failed to start several times in a row. Try again in a little while.", When: "the app hit its restart limit"},
	{Name: Maintenance, Status: http.StatusServiceUnavailable, Title: "%s is under maintenance", Message: "We will be back shortly.", When: "maintenance mode is on"},
	{Name: Error, Status: http.StatusBadGateway, Title: "Something went wrong", Message: "The app could not answer this request. Try again in a moment.", When: "the app is unreachable or answers 5xx"},
	{Name: Forbidden, Status: http.StatusForbidden, Title: "Access denied", Message: "Your address is not allowed to reach this site.", When: "allow_ips turns the visitor away"},
	{Name: SignedOut, Status: http.StatusOK, Title: "Signed out", Message: "You have been signed out of %s.", When: "after sign-out"},
	{Name: NotFound, Status: http.StatusNotFound, Title: "Nothing here", Message: "No site is configured for this address.", Host: true, When: "no app owns the host"},
	{Name: Login, Status: http.StatusUnauthorized, Title: "Sign in from the command line", Message: "Run dboss login on this host and open the link it prints. The link works once and expires after 3 minutes.", Host: true, When: "the console is opened without a session"},
}

//go:embed template.html logo.svg
var builtin embed.FS

// Logo is the dboss mark, served at LogoPath and as the console favicon.
var Logo = mustRead("logo.svg")

// Template is the built-in page every lookup falls back to.
var Template = mustRead(TemplateFile)

func mustRead(name string) []byte {
	data, err := builtin.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return data
}

// Lookup finds the file that renders name: <dir>/<name>.html, then <dir>/template.html, for each
// dir in order. Empty dirs are skipped. It returns the path it read, or "" when none exists.
func Lookup(name Name, dirs ...string) (string, []byte) {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, file := range []string{string(name) + ".html", TemplateFile} {
			path := filepath.Join(dir, file)
			if data, err := os.ReadFile(path); err == nil {
				return path, data
			}
		}
	}
	return "", nil
}

// Find returns the spec of name.
func Find(name Name) (Spec, bool) {
	for _, spec := range Specs {
		if spec.Name == name {
			return spec, true
		}
	}
	return Spec{}, false
}

// Page is one page ready to render: which one, for which app, and the status and action the
// request needs. A zero Status keeps the page's own.
type Page struct {
	Name   Name
	App    string
	Status int
	// Action is HTML dboss builds itself, such as the start button; it is not escaped.
	Action string
}

// Render reads the page from dirs (falling back to the built-in template) and fills its tokens.
func (p Page) Render(dirs ...string) []byte {
	_, data := Lookup(p.Name, dirs...)
	if data == nil {
		data = Template
	}
	return p.Fill(data)
}

// Fill replaces the known tokens in page, so a {{x}} of the page's own survives.
func (p Page) Fill(page []byte) []byte {
	spec, _ := Find(p.Name)
	status := p.StatusCode()
	app := p.App
	if app == "" {
		app = "This site"
	}
	escape := func(format string) string {
		if strings.Contains(format, "%s") {
			format = strings.ReplaceAll(format, "%s", app)
		}
		return html.EscapeString(format)
	}
	return []byte(strings.NewReplacer(
		"{{status}}", strconv.Itoa(status),
		"{{title}}", escape(spec.Title),
		"{{message}}", escape(spec.Message),
		"{{action}}", p.Action,
		"{{app}}", html.EscapeString(p.App),
		"{{dboss_logo}}", LogoPath,
	).Replace(string(page)))
}

// StatusCode is the status the page is sent with.
func (p Page) StatusCode() int {
	if p.Status != 0 {
		return p.Status
	}
	spec, _ := Find(p.Name)
	return spec.Status
}

// Write sends the rendered page with its status.
func (p Page) Write(w http.ResponseWriter, dirs ...string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.StatusCode())
	_, _ = w.Write(p.Render(dirs...))
}

// ServeLogo answers LogoPath.
func ServeLogo(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(Logo)
}

// Source is the file `dboss pages dump` writes for name: the built-in template with the page's
// wording written in, so it can be edited, and the other tokens kept. An empty name is the
// template itself.
func Source(name Name) []byte {
	spec, ok := Find(name)
	if !ok {
		return Template
	}
	text := func(format string) string {
		return strings.ReplaceAll(html.EscapeString(format), "%s", "{{app}}")
	}
	return []byte(strings.NewReplacer("{{title}}", text(spec.Title), "{{message}}", text(spec.Message)).Replace(string(Template)))
}
