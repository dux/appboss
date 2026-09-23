package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/authcog"
	"dboss/internal/supervisor"
)

const gated = "auth:\n  allow_emails: [ana@example.com, \"*@team.test\"]\n"

// signInHandler is the feature handler with a flow whose AuthCog exchange answers email.
func signInHandler(email string) *Handler {
	handler := featureHandler()
	handler.signin = authcog.NewWithKey([]byte("01234567890123456789012345678901"))
	handler.signin.Exchange = func(context.Context, string, string, string) (authcog.Profile, error) {
		return authcog.Profile{Email: email}, nil
	}
	return handler
}

func browser(target string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Accept", "text/html")
	return request
}

// signInCookieFor walks the redirect and the callback and returns the session cookie.
func signInCookieFor(t *testing.T, handler *Handler, snapshot supervisor.Snapshot) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	start := serveFeature(t, handler, snapshot, browser("http://demo.test:8080/reports?page=2"))
	login, err := url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound || login.Host != "auth.authcog.com" || login.Path != "/d:demo.test/p:8080/s:http" {
		t.Fatalf("unexpected sign-in redirect: %d %s", start.Code, start.Header().Get("Location"))
	}
	callback := browser("http://demo.test:8080" + signInCallbackPath + "?callback=verified&state=" + url.QueryEscape(login.Query().Get("state")))
	for _, cookie := range start.Result().Cookies() {
		callback.AddCookie(cookie)
	}
	response := serveFeature(t, handler, snapshot, callback)
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == signInCookie {
			return response, cookie
		}
	}
	return response, nil
}

func TestSignInGatesTheAppBehindAuthCog(t *testing.T) {
	snapshot := featureSnapshot(t, "    static: ./public\n"+gated)
	if err := os.MkdirAll(filepath.Join(snapshot.Dir, "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot.Dir, "public", "app.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stopped and without a manager: a request that got past the gate would panic in the wake stage.
	snapshot.State = supervisor.Stopped
	handler := signInHandler("Ana@Example.com")

	api := httptest.NewRequest(http.MethodGet, "http://demo.test:8080/api/items", nil)
	api.Header.Set("Accept", "application/json")
	if response := serveFeature(t, handler, snapshot, api); response.Code != http.StatusUnauthorized {
		t.Fatalf("API request without a session = %d", response.Code)
	}
	if response := serveFeature(t, handler, snapshot, httptest.NewRequest(http.MethodGet, "http://demo.test:8080/app.css", nil)); response.Code != http.StatusUnauthorized {
		t.Fatalf("static file without a session = %d", response.Code)
	}
	health := httptest.NewRequest(http.MethodGet, "http://demo.test:8080/.well-known/dboss/health", nil)
	if response := serveFeature(t, handler, snapshot, health); response.Code != http.StatusOK {
		t.Fatalf("health in front of the gate = %d", response.Code)
	}

	callback, cookie := signInCookieFor(t, handler, snapshot)
	if cookie == nil || callback.Code != http.StatusSeeOther || callback.Header().Get("Location") != "/reports?page=2" {
		t.Fatalf("unexpected callback: %d %s", callback.Code, callback.Header().Get("Location"))
	}

	static := httptest.NewRequest(http.MethodGet, "http://demo.test:8080/app.css", nil)
	static.AddCookie(cookie)
	if response := serveFeature(t, handler, snapshot, static); response.Code != http.StatusOK || response.Body.String() != "body{}" {
		t.Fatalf("static file with a session = %d %s", response.Code, response.Body.String())
	}

	// The same cookie does not open another app, and not this one once the email is removed.
	other := snapshot
	other.Name = "blog"
	if response := serveFeature(t, handler, other, static); response.Code != http.StatusUnauthorized {
		t.Fatalf("session reused on another app = %d", response.Code)
	}
	removed := featureSnapshot(t, "auth:\n  allow_emails: [someone@else.test]\n")
	removed.State = supervisor.Stopped
	if response := serveFeature(t, handler, removed, static); response.Code != http.StatusUnauthorized {
		t.Fatalf("session of a removed email = %d", response.Code)
	}

	logout := browser("http://demo.test:8080" + signInLogoutPath)
	logout.AddCookie(cookie)
	response := serveFeature(t, handler, snapshot, logout)
	if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Set-Cookie"), signInCookie+"=;") {
		t.Fatalf("logout: %d %s", response.Code, response.Header().Get("Set-Cookie"))
	}
}

func TestSignInAllowsDomainsAndRejectsOthers(t *testing.T) {
	snapshot := featureSnapshot(t, gated)
	snapshot.State = supervisor.Stopped
	if _, cookie := signInCookieFor(t, signInHandler("bo@team.test"), snapshot); cookie == nil {
		t.Fatal("an email of an allowed domain was rejected")
	}
	response, cookie := signInCookieFor(t, signInHandler("eve@example.com"), snapshot)
	if cookie != nil || response.Code != http.StatusForbidden {
		t.Fatalf("an email off the list: %d cookie %v", response.Code, cookie)
	}
}

// The app may trust X-Dboss-User: a client cannot set it, gated app or not.
func TestSignInOwnsTheUserHeader(t *testing.T) {
	handler := signInHandler("ana@example.com")
	seen := ""
	handler.filters = []Filter{handler.signIn, func(_ http.ResponseWriter, r *http.Request, _ supervisor.Snapshot, _ func()) {
		seen = r.Header.Get(userHeader)
	}}

	open := featureSnapshot(t, "")
	spoofed := browser("http://demo.test:8080/")
	spoofed.Header.Set(userHeader, "admin@example.com")
	serveFeature(t, handler, open, spoofed)
	if seen != "" {
		t.Fatalf("open app received a client supplied %s: %q", userHeader, seen)
	}

	snapshot := featureSnapshot(t, gated)
	full := signInHandler("ana@example.com")
	_, cookie := signInCookieFor(t, full, snapshot)
	handler.signin = full.signin
	request := browser("http://demo.test:8080/")
	request.Header.Set(userHeader, "admin@example.com")
	request.AddCookie(cookie)
	serveFeature(t, handler, snapshot, request)
	if seen != "ana@example.com" {
		t.Fatalf("%s = %q, want the signed-in email", userHeader, seen)
	}
}

// A pubsub publisher has the publish secret, not a browser session.
func TestSignInHonorsPublishAuthorizer(t *testing.T) {
	authorizer := &fakeAuthorizer{allow: true}
	handler := signInHandler("ana@example.com")
	handler.pubsub = authorizer
	passed := false
	handler.filters = []Filter{handler.signIn, func(http.ResponseWriter, *http.Request, supervisor.Snapshot, func()) { passed = true }}
	serveFeature(t, handler, featureSnapshot(t, gated), httptest.NewRequest(http.MethodPost, "http://demo.test:8080/socketio/news", nil))
	if !authorizer.called || !passed {
		t.Fatalf("publish secret did not pass the gate: called=%v passed=%v", authorizer.called, passed)
	}
}
