package pubsub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"dboss/internal/super"
)

func (s *Service) serveSSE(w http.ResponseWriter, r *http.Request, app string, web super.WebProcessSnapshot, channel string) {
	sub, backlog, err := s.subscribe(hubID{app, web.Name}, channel, web.Pubsub)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.unsubscribe(hubID{app, web.Name}, channel, sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	// The stream is live: keep the request out of the request log and its latency.
	suppressRecord(w)
	flusher.Flush()

	write := func(msg Message) bool {
		data, err := json.Marshal(msg)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, msg := range backlog {
		if !write(msg) {
			return
		}
	}

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.quit:
			return
		case msg := <-sub.out:
			if !write(msg) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
