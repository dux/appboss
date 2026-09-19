package pubsub

import (
	"html"
	"io"
	"net/http"
	"strings"

	"app-boss/internal/config"
)

func (s *Service) serveTestPage(w http.ResponseWriter, r *http.Request, cfg config.Pubsub) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, strings.ReplaceAll(string(testHTML), "__PATH__", html.EscapeString(cfg.Path)))
}
