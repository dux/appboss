package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuditEndpointListsRows(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	cookie, _ := sessionCookie(t, handler)
	request := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/api/audit", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"action":"restart"`) || !strings.Contains(response.Body.String(), `"actor":"admin@example.com"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestAuditEndpointRequiresSession(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	request := httptest.NewRequest(http.MethodGet, "http://boss.lvh.me:8081/api/audit", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code == http.StatusOK {
		t.Fatal("audit answered without a session")
	}
}
