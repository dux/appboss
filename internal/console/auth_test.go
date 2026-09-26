package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"dboss/internal/authcog"
	"dboss/internal/config"
)

func TestCLILoginLinkSignsInOnce(t *testing.T) {
	auth := testAuthenticator("dboss.lvh.me")
	handler := &Handler{auth: auth, managementPort: "3100", publicHost: "dboss.lvh.me"}
	link, public, err := handler.LoginURL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "http://127.0.0.1:3100/login?token=") {
		t.Fatalf("unexpected login URL: %s", link)
	}
	token := strings.TrimPrefix(link, "http://127.0.0.1:3100/login?token=")
	if public != "https://dboss.lvh.me/login?token="+token {
		t.Fatalf("unexpected public login URL: %s", public)
	}

	loginRequest := httptest.NewRequest(http.MethodGet, link, nil)
	loginResponse := httptest.NewRecorder()
	if _, ok := auth.authenticate(loginResponse, loginRequest); ok {
		t.Fatal("login request reached the console")
	}
	if loginResponse.Code != http.StatusSeeOther || loginResponse.Header().Get("Location") != "/" {
		t.Fatalf("unexpected login response: %d %s", loginResponse.Code, loginResponse.Header().Get("Location"))
	}
	sessionCookie := cookieNamed(t, loginResponse.Result().Cookies(), authSessionCookie)

	authorizedRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3100/", nil)
	authorizedRequest.AddCookie(sessionCookie)
	session, ok := auth.authenticate(httptest.NewRecorder(), authorizedRequest)
	if !ok || session.Email != cliEmail {
		t.Fatalf("cli session was not accepted: %+v %v", session, ok)
	}

	reusedResponse := httptest.NewRecorder()
	auth.authenticate(reusedResponse, httptest.NewRequest(http.MethodGet, link, nil))
	if reusedResponse.Code != http.StatusBadRequest {
		t.Fatalf("reused link status = %d", reusedResponse.Code)
	}

	expired, err := auth.issueCLIToken()
	if err != nil {
		t.Fatal(err)
	}
	auth.cliTokens[expired] = cliToken{expiresAt: time.Now().Add(-time.Second)}
	expiredResponse := httptest.NewRecorder()
	auth.authenticate(expiredResponse, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8080/login?token="+url.QueryEscape(expired), nil))
	if expiredResponse.Code != http.StatusBadRequest {
		t.Fatalf("expired link status = %d", expiredResponse.Code)
	}
}

func TestLoopbackHostOnlySignsInThroughCLI(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	for _, host := range []string{"127.0.0.1:3100", "localhost:3100", "[::1]:3100"} {
		if !loopbackHost(host) {
			t.Errorf("%s should be a loopback host", host)
		}
	}
	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3100/", nil))
	if anonymous.Code != http.StatusUnauthorized || !strings.Contains(anonymous.Body.String(), "dboss login") {
		t.Fatalf("anonymous loopback request = %d %q", anonymous.Code, anonymous.Body.String())
	}
	other := httptest.NewRecorder()
	handler.ServeHTTP(other, httptest.NewRequest(http.MethodGet, "http://evil.example/", nil))
	if other.Code != http.StatusNotFound {
		t.Fatalf("foreign host status = %d", other.Code)
	}

	link, public, err := handler.LoginURL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "http://127.0.0.1:3100/login?token=") {
		t.Fatalf("unexpected login URL: %s", link)
	}
	if !strings.HasPrefix(public, "https://dboss.lvh.me/login?token=") {
		t.Fatalf("unexpected public login URL: %s", public)
	}
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, httptest.NewRequest(http.MethodGet, link, nil))
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d", login.Code)
	}
	bootstrap := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3100/api/bootstrap", nil)
	bootstrap.AddCookie(cookieNamed(t, login.Result().Cookies(), authSessionCookie))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, bootstrap)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"viewer":"cli@localhost"`) {
		t.Fatalf("bootstrap over loopback = %d %s", response.Code, response.Body.String())
	}
}

func TestDevSignsInLoopbackPeer(t *testing.T) {
	auth := devAuthenticator(true, "dboss.lvh.me")
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3100/api/bootstrap", nil)
	request.RemoteAddr = "127.0.0.1:54321"
	session, ok := auth.authenticate(httptest.NewRecorder(), request)
	if !ok || session.Email != cliEmail || session.CSRF == "" {
		t.Fatalf("dev loopback request = %v %+v", ok, session)
	}
	// The console reads its CSRF token once and polls for the rest of the run, so the session
	// it is handed has to stay the same one.
	again, _ := auth.authenticate(httptest.NewRecorder(), request)
	if again.CSRF != session.CSRF {
		t.Fatalf("CSRF changed between requests: %q then %q", session.CSRF, again.CSRF)
	}
}

func TestDevStillAuthenticatesRemotePeer(t *testing.T) {
	auth := devAuthenticator(true, "dboss.lvh.me")
	api := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me/api/bootstrap", nil)
	api.RemoteAddr = "203.0.113.7:54321"
	response := httptest.NewRecorder()
	if _, ok := auth.authenticate(response, api); ok || response.Code != http.StatusUnauthorized {
		t.Fatalf("remote API request = %v %d", ok, response.Code)
	}
	page := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me/", nil)
	page.RemoteAddr = "203.0.113.7:54321"
	redirect := httptest.NewRecorder()
	if _, ok := auth.authenticate(redirect, page); ok || redirect.Code != http.StatusFound {
		t.Fatalf("remote page request = %v %d", ok, redirect.Code)
	}
	// A loopback host header from a remote peer is not local.
	forged := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3100/api/bootstrap", nil)
	forged.RemoteAddr = "203.0.113.7:54321"
	spoofed := httptest.NewRecorder()
	if _, ok := auth.authenticate(spoofed, forged); ok {
		t.Fatal("a forged loopback host signed a remote peer in")
	}
}

func TestAuthCogRejectsCLIEmail(t *testing.T) {
	auth := testAuthenticator("dboss.lvh.me")
	auth.flow.Exchange = func(_ context.Context, _, _, _ string) (authcog.Profile, error) {
		return authcog.Profile{Email: cliEmail}, nil
	}
	response := httptest.NewRecorder()
	auth.authenticate(response, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/", nil))
	login, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	callbackRequest := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/authcog?callback=verified-callback&state="+url.QueryEscape(login.Query().Get("state")), nil)
	callbackRequest.AddCookie(cookieNamed(t, response.Result().Cookies(), authStateCookie))
	callbackResponse := httptest.NewRecorder()
	auth.authenticate(callbackResponse, callbackRequest)
	if callbackResponse.Code != http.StatusForbidden {
		t.Fatalf("AuthCog callback with %s status = %d", cliEmail, callbackResponse.Code)
	}
}

func TestAuthCogLoginAndSession(t *testing.T) {
	auth := testAuthenticator("dboss.lvh.me")
	auth.flow.Exchange = func(_ context.Context, _, destination, callback string) (authcog.Profile, error) {
		if destination != "/d:dboss.lvh.me/p:8081/s:http" || callback != "verified-callback" {
			t.Fatalf("unexpected exchange: %s %s", destination, callback)
		}
		return authcog.Profile{Email: "admin@example.com"}, nil
	}

	request := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/?view=fleet", nil)
	response := httptest.NewRecorder()
	if _, ok := auth.authenticate(response, request); ok {
		t.Fatal("unauthenticated request was allowed")
	}
	if response.Code != http.StatusFound {
		t.Fatalf("login status = %d", response.Code)
	}
	login, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if login.Host != "auth.authcog.com" || login.Path != "/d:dboss.lvh.me/p:8081/s:http" || login.Query().Get("redirect_to") != "/?view=fleet" {
		t.Fatalf("unexpected login URL: %s", login)
	}
	state := login.Query().Get("state")
	stateCookie := cookieNamed(t, response.Result().Cookies(), authStateCookie)

	callbackRequest := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/authcog?callback=verified-callback&state="+url.QueryEscape(state), nil)
	callbackRequest.AddCookie(stateCookie)
	callbackResponse := httptest.NewRecorder()
	if _, ok := auth.authenticate(callbackResponse, callbackRequest); ok {
		t.Fatal("callback request reached the console")
	}
	if callbackResponse.Code != http.StatusSeeOther || callbackResponse.Header().Get("Location") != "/?view=fleet" {
		t.Fatalf("unexpected callback response: %d %s", callbackResponse.Code, callbackResponse.Header().Get("Location"))
	}
	sessionCookie := cookieNamed(t, callbackResponse.Result().Cookies(), authSessionCookie)

	authorizedRequest := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/", nil)
	authorizedRequest.AddCookie(sessionCookie)
	session, ok := auth.authenticate(httptest.NewRecorder(), authorizedRequest)
	if !ok || session.Email != "admin@example.com" || session.CSRF == "" {
		t.Fatalf("valid session was rejected: %+v", session)
	}

	reusedRequest := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/authcog?callback=verified-callback&state="+url.QueryEscape(state), nil)
	reusedRequest.AddCookie(stateCookie)
	reusedResponse := httptest.NewRecorder()
	auth.authenticate(reusedResponse, reusedRequest)
	if reusedResponse.Code != http.StatusBadRequest || !strings.Contains(reusedResponse.Body.String(), "expired") {
		t.Fatalf("reused callback response: %d %s", reusedResponse.Code, reusedResponse.Body.String())
	}
}

func TestAPIAuthenticationFailureIsJSON(t *testing.T) {
	auth := testAuthenticator("dboss.lvh.me")
	request := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/api/apps", nil)
	response := httptest.NewRecorder()
	if _, ok := auth.authenticate(response, request); ok {
		t.Fatal("unauthenticated API request was allowed")
	}
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "authentication required") {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
}

// testAuthenticator is the console gate for the given management hosts with one admin and a
// fixed signing key, so no test touches the disk.
func testAuthenticator(hosts ...string) *authenticator {
	return devAuthenticator(false, hosts...)
}

// devAuthenticator is the same gate with the dev flag set either way.
func devAuthenticator(dev bool, hosts ...string) *authenticator {
	cfg := config.Default()
	cfg.Management = config.Management{Host: hosts, Admins: []string{"admin@example.com"}}
	cfg.Defaults.SessionTTL = config.Duration(time.Hour)
	if dev {
		cfg.App = &config.App{}
	}
	auth, err := consoleAuthenticator(authcog.NewWithKey([]byte("01234567890123456789012345678901")), cfg, func() string { return "" })
	if err != nil {
		panic(err)
	}
	return auth
}

func cookieNamed(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("missing cookie %s", name)
	return nil
}

// The AuthCog callback lands inside a cross-site navigation; a Strict session cookie would be
// withheld on the redirect that follows and every login would loop back to the login page.
func TestSessionCookieIsLaxForCrossSiteCallback(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	response := httptest.NewRecorder()
	if err := handler.auth.setSessionCookie(response, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/authcog", nil), "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	cookie := cookieNamed(t, response.Result().Cookies(), authSessionCookie)
	if cookie.SameSite != http.SameSiteLaxMode || !cookie.HttpOnly {
		t.Fatalf("session cookie = %+v", cookie)
	}
}
