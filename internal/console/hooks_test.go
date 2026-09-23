package console

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dboss/internal/supervisor"
)

func hookSignature(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestHookEndpointAcceptsQueryToken(t *testing.T) {
	manager := &fakeManager{hookSecrets: map[string]string{"sinatra/deploy": "tok3n"}}
	handler := newTestHandler(t, manager, nil)
	request := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/hooks/sinatra/deploy?token=tok3n", strings.NewReader(`{"ref":"main"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if len(manager.actions) != 1 || manager.actions[0] != "hook-run sinatra deploy" {
		t.Fatalf("actions = %v", manager.actions)
	}
}

func TestHookEndpointAcceptsGitHubSignature(t *testing.T) {
	body := `{"ref":"refs/heads/main"}`
	manager := &fakeManager{hookSecrets: map[string]string{"sinatra/deploy": "s3cret"}}
	handler := newTestHandler(t, manager, nil)
	request := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/hooks/sinatra/deploy", strings.NewReader(body))
	request.Header.Set("X-Hub-Signature-256", hookSignature("s3cret", body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}

func TestHookEndpointRejectsBadSecret(t *testing.T) {
	manager := &fakeManager{hookSecrets: map[string]string{"sinatra/deploy": "s3cret"}}
	handler := newTestHandler(t, manager, nil)
	request := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/hooks/sinatra/deploy?token=wrong", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
	if len(manager.actions) != 0 {
		t.Fatalf("action ran with a bad secret: %v", manager.actions)
	}
}

func TestHookEndpointAnswersPing(t *testing.T) {
	body := `{"zen":"keep it simple"}`
	manager := &fakeManager{hookSecrets: map[string]string{"sinatra/deploy": "s3cret"}}
	handler := newTestHandler(t, manager, nil)
	request := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/hooks/sinatra/deploy", strings.NewReader(body))
	request.Header.Set("X-Hub-Signature-256", hookSignature("s3cret", body))
	request.Header.Set("X-GitHub-Event", "ping")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if len(manager.actions) != 0 {
		t.Fatalf("ping ran the hook: %v", manager.actions)
	}
}

func TestHookEndpointHidesUnknownHook(t *testing.T) {
	manager := &fakeManager{hookSecrets: map[string]string{"sinatra/deploy": "s3cret"}}
	handler := newTestHandler(t, manager, nil)
	request := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/hooks/sinatra/missing?token=s3cret", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestHookEndpointOnlyAnswersOnTheManagementHost(t *testing.T) {
	manager := &fakeManager{hookSecrets: map[string]string{"sinatra/deploy": "tok"}}
	handler := newTestHandler(t, manager, nil)
	request := httptest.NewRequest(http.MethodPost, "http://other.lvh.me/hooks/sinatra/deploy?token=tok", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestHookAPIRequiresSession(t *testing.T) {
	manager := &fakeManager{hooks: map[string][]supervisor.HookInfo{"sinatra": {{HookSnapshot: supervisor.HookSnapshot{Name: "deploy"}}}}}
	handler := newTestHandler(t, manager, nil)
	request := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/api/hooks?app=sinatra", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code == http.StatusOK {
		t.Fatal("hooks API answered without a session")
	}
}
