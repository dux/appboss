package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"dboss/internal/authcog"
	"dboss/internal/super"
)

const authCogEnabled = "authcog:\n  login: true\n"

// authCogHandler is the feature handler with a flow whose exchange answers a full profile.
func authCogHandler(t *testing.T, profile authcog.Profile) *Handler {
	t.Helper()
	handler := featureHandler()
	handler.signin = authcog.NewWithKey([]byte("01234567890123456789012345678901"))
	handler.signin.Exchange = func(_ context.Context, realm, destination, callback string) (authcog.Profile, error) {
		if realm == "" || destination == "" || callback == "" {
			t.Fatalf("unexpected exchange: realm=%q destination=%q callback=%q", realm, destination, callback)
		}
		return profile, nil
	}
	return handler
}

// authCogCapture replaces the pipeline with the authcog stage and a capture, so the handoff can
// be inspected without a manager to forward it to.
type authCogCapture struct{ request *http.Request }

func captureAuthCog(handler *Handler) *authCogCapture {
	capture := &authCogCapture{}
	handler.filters = []Filter{handler.authCog, func(_ http.ResponseWriter, r *http.Request, _ super.Snapshot, _ func()) {
		capture.request = r
	}}
	return capture
}

func TestAuthCogHandsTheProfileToTheApp(t *testing.T) {
	handler := authCogHandler(t, authcog.Profile{Email: "Ana@Example.com", Name: "Ana", Avatar: "https://img.test/a.png", Provider: "google"})
	snapshot := featureSnapshot(t, authCogEnabled)
	capture := captureAuthCog(handler)

	start := serveFeature(t, handler, snapshot, browser("http://demo.test:8080/authcog"))
	login, err := url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound || login.Host != "auth.authcog.com" || login.Path != "/d:demo.test/p:8080" {
		t.Fatalf("unexpected login redirect: %d %s", start.Code, start.Header().Get("Location"))
	}
	if state := login.Query().Get("state"); state == "" {
		t.Fatal("login URL is missing the state challenge")
	}

	callback := browser("http://demo.test:8080/authcog?callback=verified&state=" + url.QueryEscape(login.Query().Get("state")))
	for _, cookie := range start.Result().Cookies() {
		callback.AddCookie(cookie)
	}
	if response := serveFeature(t, handler, snapshot, callback); response.Code != http.StatusOK {
		t.Fatalf("callback = %d", response.Code)
	}
	handed := capture.request
	if handed == nil {
		t.Fatal("the app was never handed the request")
	}
	if handed.URL.Path != "/authcog" || handed.URL.RawQuery != "" {
		t.Fatalf("app saw %q, want the bare /authcog path", handed.URL.RequestURI())
	}
	var profile authcog.Profile
	if err := json.Unmarshal([]byte(handed.Header.Get(userHeader)), &profile); err != nil {
		t.Fatalf("X-Dboss-User is not profile JSON: %q", handed.Header.Get(userHeader))
	}
	if profile.Email != "ana@example.com" || profile.Name != "Ana" || profile.Avatar != "https://img.test/a.png" || profile.Provider != "google" {
		t.Fatalf("unexpected profile: %+v", profile)
	}
}

func TestAuthCogRealmAndPathAreConfigurable(t *testing.T) {
	handler := authCogHandler(t, authcog.Profile{Email: "ana@example.com"})
	snapshot := featureSnapshot(t, "authcog:\n  login: true\n  realm: foo\n  path: /sign-in\n")
	captureAuthCog(handler)
	start := serveFeature(t, handler, snapshot, browser("http://demo.test:8080/sign-in"))
	login, _ := url.Parse(start.Header().Get("Location"))
	if login.Host != "foo.authcog.com" {
		t.Fatalf("realm ignored: %s", login)
	}
	if response := serveFeature(t, handler, snapshot, browser("http://demo.test:8080/authcog")); response.Code != http.StatusOK {
		t.Fatalf("the old default path should pass through, got %d", response.Code)
	}
}

func TestAuthCogRejectsNonGet(t *testing.T) {
	handler := authCogHandler(t, authcog.Profile{Email: "ana@example.com"})
	response := serveFeature(t, handler, featureSnapshot(t, authCogEnabled), httptest.NewRequest(http.MethodPost, "http://demo.test:8080/authcog", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /authcog = %d, want 405", response.Code)
	}
}

// A client cannot forge the identity, for an app with the feature on or off.
func TestAuthCogStripsInboundUserHeader(t *testing.T) {
	for name, body := range map[string]string{"enabled": authCogEnabled, "disabled": ""} {
		handler := authCogHandler(t, authcog.Profile{Email: "ana@example.com"})
		capture := captureAuthCog(handler)
		request := browser("http://demo.test:8080/")
		request.Header.Set(userHeader, "admin@example.com")
		serveFeature(t, handler, featureSnapshot(t, body), request)
		if capture.request == nil {
			t.Fatalf("%s app request never reached the capture", name)
		}
		if capture.request.Header.Get(userHeader) != "" {
			t.Fatalf("%s app received a client supplied %s: %q", name, userHeader, capture.request.Header.Get(userHeader))
		}
	}
}
