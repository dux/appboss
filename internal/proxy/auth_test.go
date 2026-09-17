package proxy

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

func TestAuthCogLoginAndSession(t *testing.T) {
	auth := &authenticator{
		cfg:        config.Auth{Realm: "auth.authcog.com", AdminEmails: []string{"admin@example.com"}, SessionTTL: config.Duration(time.Hour)},
		key:        []byte("01234567890123456789012345678901"),
		admins:     map[string]bool{"admin@example.com": true},
		challenges: map[string]authChallenge{},
	}
	auth.exchange = func(_ context.Context, destination, callback string) (authProfile, error) {
		if destination != "/d:sinatra.lvh.me/p:8080" || callback != "verified-callback" {
			t.Fatalf("unexpected exchange: %s %s", destination, callback)
		}
		return authProfile{Email: "admin@example.com"}, nil
	}

	request := httptest.NewRequest(http.MethodGet, "http://sinatra.lvh.me:8080/private?x=1", nil)
	response := httptest.NewRecorder()
	if auth.authorize(response, request) {
		t.Fatal("unauthenticated request was allowed")
	}
	if response.Code != http.StatusFound {
		t.Fatalf("login status = %d", response.Code)
	}
	login, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if login.Host != "auth.authcog.com" || login.Path != "/d:sinatra.lvh.me/p:8080" || login.Query().Get("redirect_to") != "/private?x=1" {
		t.Fatalf("unexpected login URL: %s", login)
	}
	state := login.Query().Get("state")
	stateCookie := cookieNamed(t, response.Result().Cookies(), authStateCookie)

	callbackRequest := httptest.NewRequest(http.MethodGet, "http://sinatra.lvh.me:8080/authcog?callback=verified-callback&state="+url.QueryEscape(state), nil)
	callbackRequest.AddCookie(stateCookie)
	callbackResponse := httptest.NewRecorder()
	if auth.authorize(callbackResponse, callbackRequest) {
		t.Fatal("callback request was passed upstream")
	}
	if callbackResponse.Code != http.StatusSeeOther || callbackResponse.Header().Get("Location") != "/private?x=1" {
		t.Fatalf("unexpected callback response: %d %s", callbackResponse.Code, callbackResponse.Header().Get("Location"))
	}
	sessionCookie := cookieNamed(t, callbackResponse.Result().Cookies(), authSessionCookie)

	authorizedRequest := httptest.NewRequest(http.MethodGet, "http://sinatra.lvh.me:8080/private", nil)
	authorizedRequest.AddCookie(sessionCookie)
	if !auth.authorize(httptest.NewRecorder(), authorizedRequest) {
		t.Fatal("valid session was rejected")
	}

	reusedRequest := httptest.NewRequest(http.MethodGet, "http://sinatra.lvh.me:8080/authcog?callback=verified-callback&state="+url.QueryEscape(state), nil)
	reusedRequest.AddCookie(stateCookie)
	reusedResponse := httptest.NewRecorder()
	auth.authorize(reusedResponse, reusedRequest)
	if reusedResponse.Code != http.StatusBadRequest || !strings.Contains(reusedResponse.Body.String(), "expired") {
		t.Fatalf("reused callback response: %d %s", reusedResponse.Code, reusedResponse.Body.String())
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
