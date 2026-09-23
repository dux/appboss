package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestBearerToken(t *testing.T) {
	for header, want := range map[string]string{
		"":                   "",
		"Bearer abc":         "abc",
		"bearer abc":         "abc",
		"BEARER  abc ":       "abc",
		"Basic dXNlcjpwdw==": "",
		"Bearer":             "",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := BearerToken(r); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}
