package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type runSlack struct {
	*httptest.Server
	connections chan *websocket.Conn
	posts       chan url.Values
	acks        chan string
}

func newRunSlack(t *testing.T, beforePost func(context.Context, url.Values)) *runSlack {
	t.Helper()
	s := &runSlack{connections: make(chan *websocket.Conn, 4), posts: make(chan url.Values, 32), acks: make(chan string, 32)}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "https://api.slack.com" }}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users.info":
			fmt.Fprint(w, `{"ok":true,"user":{"profile":{"display_name":"Owner"}}}`)
		case "/conversations.info":
			require.NoError(t, r.ParseForm())
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"name": "test-room", "is_im": strings.HasPrefix(r.Form.Get("channel"), "D"), "user": "U_OWNER"}})
		case "/conversations.history", "/conversations.replies":
			fmt.Fprint(w, `{"ok":true,"messages":[]}`)
		case "/auth.test":
			_, _ = io.WriteString(w, `{"ok":true,"user_id":"U_BOT","bot_id":"B_BOT"}`)
		case "/apps.connections.open":
			require.Equal(t, "Bearer xapp-fake", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws://" + r.Host + "/socket"})
		case "/socket":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello"}`)))
			// Socket Mode requires server pings even when the conversation is silent.
			closed := make(chan struct{})
			defer close(closed)
			go func() {
				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-closed:
						return
					case <-ticker.C:
						if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)) != nil {
							return
						}
					}
				}
			}()
			s.connections <- conn
			for {
				var ack struct {
					EnvelopeID string `json:"envelope_id"`
				}
				if err := conn.ReadJSON(&ack); err != nil {
					return
				}
				s.acks <- ack.EnvelopeID
			}
		case "/assistant.threads.setStatus":
			// The model may set a thread status before it posts there.
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/conversations.open":
			_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"D_OWNER"}}`)
		case "/chat.postMessage":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "xoxb-fake", r.Form.Get("token"))
			if beforePost != nil {
				beforePost(r.Context(), r.Form)
			}
			s.posts <- r.Form
			_, _ = fmt.Fprintf(w, `{"ok":true,"channel":%q,"ts":"2.3"}`, r.Form.Get("channel"))
		default:
			t.Errorf("unexpected Slack API request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}
