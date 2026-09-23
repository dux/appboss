// Package httpx holds request helpers shared by the console, the proxy and pubsub.
package httpx

import (
	"net/http"
	"strings"
)

// BearerToken is the credential of an "Authorization: Bearer <token>" header, or "" without one.
// The scheme is case-insensitive, as RFC 7235 says.
func BearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}
