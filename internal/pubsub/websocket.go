package pubsub

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"dboss/internal/super"
)

const (
	pingInterval = 25 * time.Second
	writeTimeout = 10 * time.Second
)

func (s *Service) serveWebSocket(w http.ResponseWriter, r *http.Request, app string, web super.WebProcessSnapshot, channel string) {
	cfg := web.Pubsub
	sub, backlog, err := s.subscribe(hubID{app, web.Name}, channel, cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.unsubscribe(hubID{app, web.Name}, channel, sub)

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	if cfg.MaxMessageSize > 0 {
		conn.SetReadLimit(int64(cfg.MaxMessageSize))
	}
	// The connection is live: keep the request out of the request log and its latency.
	suppressRecord(w)

	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		// Closing here unblocks the read loop when the writer stops for a reason other than the
		// client closing, e.g. a slow subscriber that was dropped or a daemon shutdown.
		defer conn.CloseNow()
		s.writeWebSocket(ctx, conn, sub, backlog)
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		if !cfg.ClientEvents {
			continue
		}
		if msg, ok := parseClientMessage(data); ok {
			s.publish(hubID{app, web.Name}, channel, msg, cfg.Replay)
		}
	}
	cancel()
	<-done
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func (s *Service) writeWebSocket(ctx context.Context, conn *websocket.Conn, sub *subscriber, backlog []Message) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	write := func(msg Message) bool {
		data, err := json.Marshal(msg)
		if err != nil {
			return true
		}
		writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
		defer cancel()
		return conn.Write(writeCtx, websocket.MessageText, data) == nil
	}
	for _, msg := range backlog {
		if !write(msg) {
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.quit:
			return
		case msg := <-sub.out:
			if !write(msg) {
				return
			}
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
