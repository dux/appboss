package authcog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testGate(audience string, allowed ...string) Gate {
	return Gate{
		Audience:      audience,
		Realm:         "auth.authcog.com",
		Hosts:         func(host string) bool { return host == "shop.lvh.me" },
		CallbackPath:  "/.well-known/dboss/auth",
		StateCookie:   "state",
		SessionCookie: "session",
		TTL:           time.Hour,
		Allow: func(email string) bool {
			for _, entry := range allowed {
				if entry == email {
					return true
				}
			}
			return false
		},
	}
}

func testFlow(email string) *Flow {
	flow := NewWithKey([]byte("01234567890123456789012345678901"))
	flow.Exchange = func(context.Context, string, string, string) (Profile, error) { return Profile{Email: email}, nil }
	return flow
}

func cookieNamed(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("cookie %s was not set", name)
	return nil
}

// signIn walks Start and Callback and returns the callback response.
func signIn(t *testing.T, flow *Flow, gate Gate) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://shop.lvh.me:8080/cart?step=2", nil)
	start := httptest.NewRecorder()
	flow.Start(start, request, gate)
	login, err := url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound {
		t.Fatalf("start: %d %v", start.Code, err)
	}
	if login.Host != "auth.authcog.com" || login.Path != "/d:shop.lvh.me/p:8080" || login.Query().Get("redirect_to") != "/cart?step=2" {
		t.Fatalf("unexpected login URL: %s", login)
	}
	callback := httptest.NewRequest(http.MethodGet, "http://shop.lvh.me:8080"+gate.CallbackPath+"?callback=verified&state="+url.QueryEscape(login.Query().Get("state")), nil)
	callback.AddCookie(cookieNamed(t, start, gate.StateCookie))
	response := httptest.NewRecorder()
	flow.Callback(response, callback, gate)
	return response
}

func TestSignInIssuesASessionForItsGateOnly(t *testing.T) {
	flow := testFlow("Ana@Example.com")
	gate := testGate("app:shop", "ana@example.com")
	response := signIn(t, flow, gate)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/cart?step=2" {
		t.Fatalf("callback: %d %s", response.Code, response.Header().Get("Location"))
	}
	cookie := cookieNamed(t, response, gate.SessionCookie)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Secure {
		t.Fatalf("unexpected cookie attributes: %+v", cookie)
	}

	request := httptest.NewRequest(http.MethodGet, "http://shop.lvh.me:8080/", nil)
	request.AddCookie(cookie)
	session, ok := flow.Session(request, gate)
	if !ok || session.Email != "ana@example.com" || session.Audience != "app:shop" || session.CSRF == "" {
		t.Fatalf("session rejected: %+v", session)
	}
	if _, ok := flow.Session(request, testGate("app:blog", "ana@example.com")); ok {
		t.Fatal("a session must not open another gate")
	}
	tampered := httptest.NewRequest(http.MethodGet, "http://shop.lvh.me:8080/", nil)
	tampered.AddCookie(&http.Cookie{Name: gate.SessionCookie, Value: cookie.Value + "x"})
	if _, ok := flow.Session(tampered, gate); ok {
		t.Fatal("a tampered cookie was accepted")
	}
	expired := gate
	expired.TTL = -time.Minute
	stale := httptest.NewRecorder()
	if err := flow.SetSession(stale, request, expired, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	old := httptest.NewRequest(http.MethodGet, "http://shop.lvh.me:8080/", nil)
	old.AddCookie(&http.Cookie{Name: gate.SessionCookie, Value: cookieNamed(t, stale, gate.SessionCookie).Value})
	if _, ok := flow.Session(old, gate); ok {
		t.Fatal("an expired session was accepted")
	}
}

func TestAuthenticateReturnsTheProfileWithoutASession(t *testing.T) {
	flow := testFlow("Ana@Example.com")
	gate := testGate("app:shop", "ana@example.com")
	request := httptest.NewRequest(http.MethodGet, "http://shop.lvh.me:8080/", nil)
	start := httptest.NewRecorder()
	flow.Start(start, request, gate)
	login, _ := url.Parse(start.Header().Get("Location"))
	callback := httptest.NewRequest(http.MethodGet, "http://shop.lvh.me:8080"+gate.CallbackPath+"?callback=verified&state="+url.QueryEscape(login.Query().Get("state")), nil)
	callback.AddCookie(cookieNamed(t, start, gate.StateCookie))
	response := httptest.NewRecorder()
	profile, ok := flow.Authenticate(response, callback, gate)
	if !ok || profile.Email != "ana@example.com" {
		t.Fatalf("Authenticate = %+v %v", profile, ok)
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == gate.SessionCookie {
			t.Fatal("Authenticate must not set a session cookie")
		}
	}
}

func TestCallbackRejectsAnEmailTheGateDenies(t *testing.T) {
	response := signIn(t, testFlow("eve@example.com"), testGate("app:shop", "ana@example.com"))
	if response.Code != http.StatusForbidden || response.Body.String() != "User with eve@example.com is not permitted to login to dboss.\n" {
		t.Fatalf("callback: %d %s", response.Code, response.Body.String())
	}
}

func TestCallbackIsBoundToStateAndGate(t *testing.T) {
	flow := testFlow("ana@example.com")
	gate := testGate("app:shop", "ana@example.com")
	request := httptest.NewRequest(http.MethodGet, "http://shop.lvh.me:8080/", nil)
	start := httptest.NewRecorder()
	flow.Start(start, request, gate)
	login, _ := url.Parse(start.Header().Get("Location"))
	target := "http://shop.lvh.me:8080" + gate.CallbackPath + "?callback=verified&state=" + url.QueryEscape(login.Query().Get("state"))

	missing := httptest.NewRecorder()
	flow.Callback(missing, httptest.NewRequest(http.MethodGet, target, nil), gate)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("callback without the state cookie: %d", missing.Code)
	}

	other := testGate("app:blog", "ana@example.com")
	crossed := httptest.NewRequest(http.MethodGet, target, nil)
	crossed.AddCookie(cookieNamed(t, start, gate.StateCookie))
	response := httptest.NewRecorder()
	flow.Callback(response, crossed, other)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("a challenge finished on another gate: %d", response.Code)
	}
	// The challenge is single use, so the right gate cannot finish it afterwards either.
	reused := httptest.NewRequest(http.MethodGet, target, nil)
	reused.AddCookie(cookieNamed(t, start, gate.StateCookie))
	again := httptest.NewRecorder()
	flow.Callback(again, reused, gate)
	if again.Code != http.StatusBadRequest || !strings.Contains(again.Body.String(), "expired") {
		t.Fatalf("reused callback: %d %s", again.Code, again.Body.String())
	}
}

func TestDestinationAcceptsOnlyTheGateHosts(t *testing.T) {
	hosts := func(host string) bool { return strings.HasSuffix(host, "lvh.me") }
	for host, want := range map[string]string{"shop.lvh.me": "/d:shop.lvh.me", "Shop.LVH.me.:8080": "/d:shop.lvh.me/p:8080"} {
		if got, err := Destination(host, hosts); err != nil || got != want {
			t.Fatalf("Destination(%q) = %q %v, want %q", host, got, err, want)
		}
	}
	for _, host := range []string{"other.test", "a/../b.lvh.me", "shop.lvh.me:0", "shop.lvh.me:x", "", "sh op.lvh.me"} {
		if got, err := Destination(host, hosts); err == nil {
			t.Fatalf("Destination(%q) = %q, want an error", host, got)
		}
	}
}

func TestSafeRedirectRejectsAuthorityAndCallbackPaths(t *testing.T) {
	for _, target := range []string{"https://example.com", "//example.com", `/\\example.com`, "/authcog?callback=value"} {
		if got := safeRedirect(target, "/authcog"); got != "/" {
			t.Fatalf("safeRedirect(%q) = %q", target, got)
		}
	}
	if got := safeRedirect("/apps?state=running", "/authcog"); got != "/apps?state=running" {
		t.Fatalf("safe path changed to %q", got)
	}
}

func TestSecureFollowsAuthCogLocalRules(t *testing.T) {
	for _, target := range []string{"http://dboss.lvh.me/", "http://dboss.lvh.me:8081/", "http://127.0.0.1:3100/"} {
		if secure(httptest.NewRequest(http.MethodGet, target, nil)) {
			t.Fatalf("%s should use an HTTP callback", target)
		}
	}
	forwarded := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me/", nil)
	forwarded.Header.Set("X-Forwarded-Proto", "https")
	if !secure(forwarded) {
		t.Fatal("forwarded HTTPS should use an HTTPS callback")
	}
	if !secure(httptest.NewRequest(http.MethodGet, "http://dboss.example.com/", nil)) {
		t.Fatal("non-local AuthCog destination should use an HTTPS callback")
	}
}
