package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"deploy-boss/internal/config"
)

func TestCLILoginLinkSignsInOnce(t *testing.T) {
	auth := &authenticator{
		cfg:        config.ManagementAuth{Realm: "auth.authcog.com", AdminEmails: []string{"admin@example.com"}, SessionTTL: config.Duration(time.Hour)},
		host:       "boss.lvh.me",
		key:        []byte("01234567890123456789012345678901"),
		admins:     map[string]bool{"admin@example.com": true},
		challenges: map[string]authChallenge{},
	}
	handler := &Handler{auth: auth, loginPort: "8080"}
	link, err := handler.LoginURL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "http://boss.lvh.me:8080/login?token=") {
		t.Fatalf("unexpected login URL: %s", link)
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

	authorizedRequest := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8080/", nil)
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
	auth.cliTokens[expired] = time.Now().Add(-time.Second)
	expiredResponse := httptest.NewRecorder()
	auth.authenticate(expiredResponse, httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8080/login?token="+url.QueryEscape(expired), nil))
	if expiredResponse.Code != http.StatusBadRequest {
		t.Fatalf("expired link status = %d", expiredResponse.Code)
	}
}

func TestAuthCogRejectsCLIEmail(t *testing.T) {
	auth := &authenticator{
		cfg:        config.ManagementAuth{Realm: "auth.authcog.com", AdminEmails: []string{"admin@example.com"}, SessionTTL: config.Duration(time.Hour)},
		host:       "boss.lvh.me",
		key:        []byte("01234567890123456789012345678901"),
		admins:     map[string]bool{"admin@example.com": true},
		challenges: map[string]authChallenge{},
	}
	auth.exchange = func(_ context.Context, _, _ string) (authProfile, error) {
		return authProfile{Email: cliEmail}, nil
	}
	response := httptest.NewRecorder()
	auth.authenticate(response, httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/", nil))
	login, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	callbackRequest := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/authcog?callback=verified-callback&state="+url.QueryEscape(login.Query().Get("state")), nil)
	callbackRequest.AddCookie(cookieNamed(t, response.Result().Cookies(), authStateCookie))
	callbackResponse := httptest.NewRecorder()
	auth.authenticate(callbackResponse, callbackRequest)
	if callbackResponse.Code != http.StatusForbidden {
		t.Fatalf("AuthCog callback with %s status = %d", cliEmail, callbackResponse.Code)
	}
}

func TestAuthCogLoginAndSession(t *testing.T) {
	auth := &authenticator{
		cfg:        config.ManagementAuth{Realm: "auth.authcog.com", AdminEmails: []string{"admin@example.com"}, SessionTTL: config.Duration(time.Hour)},
		host:       "boss.lvh.me",
		key:        []byte("01234567890123456789012345678901"),
		admins:     map[string]bool{"admin@example.com": true},
		challenges: map[string]authChallenge{},
	}
	auth.exchange = func(_ context.Context, destination, callback string) (authProfile, error) {
		if destination != "/d:boss.lvh.me/p:8081" || callback != "verified-callback" {
			t.Fatalf("unexpected exchange: %s %s", destination, callback)
		}
		return authProfile{Email: "admin@example.com"}, nil
	}

	request := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/?view=fleet", nil)
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
	if login.Host != "auth.authcog.com" || login.Path != "/d:boss.lvh.me/p:8081" || login.Query().Get("redirect_to") != "/?view=fleet" {
		t.Fatalf("unexpected login URL: %s", login)
	}
	state := login.Query().Get("state")
	stateCookie := cookieNamed(t, response.Result().Cookies(), authStateCookie)

	callbackRequest := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/authcog?callback=verified-callback&state="+url.QueryEscape(state), nil)
	callbackRequest.AddCookie(stateCookie)
	callbackResponse := httptest.NewRecorder()
	if _, ok := auth.authenticate(callbackResponse, callbackRequest); ok {
		t.Fatal("callback request reached the console")
	}
	if callbackResponse.Code != http.StatusSeeOther || callbackResponse.Header().Get("Location") != "/?view=fleet" {
		t.Fatalf("unexpected callback response: %d %s", callbackResponse.Code, callbackResponse.Header().Get("Location"))
	}
	sessionCookie := cookieNamed(t, callbackResponse.Result().Cookies(), authSessionCookie)

	authorizedRequest := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/", nil)
	authorizedRequest.AddCookie(sessionCookie)
	session, ok := auth.authenticate(httptest.NewRecorder(), authorizedRequest)
	if !ok || session.Email != "admin@example.com" || session.CSRF == "" {
		t.Fatalf("valid session was rejected: %+v", session)
	}

	reusedRequest := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/authcog?callback=verified-callback&state="+url.QueryEscape(state), nil)
	reusedRequest.AddCookie(stateCookie)
	reusedResponse := httptest.NewRecorder()
	auth.authenticate(reusedResponse, reusedRequest)
	if reusedResponse.Code != http.StatusBadRequest || !strings.Contains(reusedResponse.Body.String(), "expired") {
		t.Fatalf("reused callback response: %d %s", reusedResponse.Code, reusedResponse.Body.String())
	}
}

func TestAPIAuthenticationFailureIsJSON(t *testing.T) {
	auth := &authenticator{host: "boss.lvh.me", admins: map[string]bool{}}
	request := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/api/apps", nil)
	response := httptest.NewRecorder()
	if _, ok := auth.authenticate(response, request); ok {
		t.Fatal("unauthenticated API request was allowed")
	}
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "authentication required") {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
}

func TestSafeRedirectRejectsAuthorityAndCallbackPaths(t *testing.T) {
	for _, target := range []string{"https://example.com", "//example.com", `/\\example.com`, "/authcog?callback=value"} {
		if got := safeRedirect(target); got != "/" {
			t.Fatalf("safeRedirect(%q) = %q", target, got)
		}
	}
	if got := safeRedirect("/apps?state=running"); got != "/apps?state=running" {
		t.Fatalf("safe path changed to %q", got)
	}
}

func TestSecureRequestFollowsAuthCogLocalRules(t *testing.T) {
	local := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/", nil)
	if secureRequest(local) {
		t.Fatal("high-port lvh.me request should use an HTTP callback")
	}
	production := httptest.NewRequest(http.MethodGet, "http://boss.example.com/", nil)
	if !secureRequest(production) {
		t.Fatal("non-local AuthCog destination should use an HTTPS callback")
	}
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
	if err := handler.auth.setSessionCookie(response, httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/authcog", nil), "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	cookie := cookieNamed(t, response.Result().Cookies(), authSessionCookie)
	if cookie.SameSite != http.SameSiteLaxMode || !cookie.HttpOnly {
		t.Fatalf("session cookie = %+v", cookie)
	}
}
