package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	sdk "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func TestRunMultipleMessagingInstances(t *testing.T) {
	dir := t.TempDir()
	binary, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("NOCTILUCA_RUN_TEST_BINARY", binary)
	t.Setenv("NOCTILUCA_RUN_TEST_DIR", dir)
	t.Setenv("NOCTILUCA_RUN_RECOVERY", "1")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexec \"$NOCTILUCA_RUN_TEST_BINARY\" -test.run=TestRunCLIHelper -- \"$@\"\n"), 0700))
	for _, name := range []string{"support", "slack"} {
		t.Setenv(name+"_APP", "xapp-"+name)
		t.Setenv(name+"_BOT", "xoxb-"+name)
	}
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`messaging:
  - name: support
    type: slack
    owner: U_SUPPORT
    slack:
      app_token: {env: support_APP}
      bot_token: {env: support_BOT}
      group_refresh_interval: 10ms
    trust: {groups: [S_SHARED], bots: [U_ALERT]}
    conversations: [{id: CSHARED, require_mention: true}]
  - name: slack
    type: slack
    owner: U_OWNER
    slack:
      app_token: {env: slack_APP}
      bot_token: {env: slack_BOT}
      group_refresh_interval: 15ms
    trust: {groups: [S_SHARED], bots: [U_ALERT]}
    conversations: [{id: CSHARED, require_mention: true}, {id: DDIRECT, ignore: true}]
model: {executable: ./claude}
heartbeat: {enabled: false}
`), 0600))
	sockets := map[string]chan *websocket.Conn{"support": make(chan *websocket.Conn, 1), "slack": make(chan *websocket.Conn, 1)}
	posts := make(chan url.Values, 8)
	owners := map[string]string{"support": "U_SUPPORT", "slack": "U_OWNER"}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "https://api.slack.com" }}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := strings.CutPrefix(r.URL.Path, "/socket/"); ok {
			conn, err := upgrader.Upgrade(w, r, nil)
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.WriteJSON(map[string]any{"type": "hello"}))
			sockets[name] <- conn
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}
		require.NoError(t, r.ParseForm())
		w.Header().Set("Content-Type", "application/json")
		name := strings.TrimPrefix(r.Form.Get("token"), "xoxb-")
		if r.URL.Path == "/apps.connections.open" {
			name = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer xapp-")
		}
		require.Contains(t, owners, name, "unexpected token at %s", r.URL.Path)
		switch r.URL.Path {
		case "/auth.test":
			fmt.Fprint(w, `{"ok":true,"user_id":"U_BOT","bot_id":"B_BOT"}`)
		case "/apps.connections.open":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws://" + r.Host + "/socket/" + name})
		case "/usergroups.users.list":
			require.Equal(t, "S_SHARED", r.Form.Get("usergroup"))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "users": []string{owners[name], "U_MEMBER"}})
		case "/users.info":
			fmt.Fprint(w, `{"ok":true,"user":{"profile":{"display_name":"Owner"}}}`)
		case "/conversations.info":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"name": "shared-room", "is_im": strings.HasPrefix(r.Form.Get("channel"), "D"), "user": owners[name]}})
		case "/conversations.open":
			require.Equal(t, owners[name], r.Form.Get("users"))
			fmt.Fprint(w, `{"ok":true,"channel":{"id":"D_OWNER"}}`)
		case "/chat.postMessage":
			posts <- r.Form
			_, _ = fmt.Fprintf(w, `{"ok":true,"channel":%q,"ts":"2.3"}`, r.Form.Get("channel"))
		default:
			t.Errorf("unexpected Slack request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	c, err := loadConfig(path)
	require.NoError(t, err)
	app, err := newApplication(t.Context(), c, nil, sdk.OptionAPIURL(api.URL+"/"))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.Start(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("messaging entry shutdown timed out")
		}
	}()
	takePost := func() url.Values {
		t.Helper()
		select {
		case post := <-posts:
			return post
		case <-time.After(10 * time.Second):
			t.Fatal("reply timed out")
			return nil
		}
	}
	for _, name := range []string{"support", "slack"} {
		var socket *websocket.Conn
		select {
		case socket = <-sockets[name]:
		case <-time.After(5 * time.Second):
			t.Fatal("socket startup timed out", name)
		}
		for i, tc := range []struct {
			channel, user, bot string
			accepted           bool
		}{
			{"CSHARED", owners[name], "", true},
			{"DDIRECT", owners[name], "", name == "support"},
			{"CSHARED", "U_MEMBER", "", true},
			{"C_UNLISTED", "U_MEMBER", "", true},
			{"D_FRIEND", "U_MEMBER", "", true},
			{"C_UNLISTED", "U_ALERT", "B_ALERT", true},
			{"C_UNLISTED", "U_MEMBER", "B_OTHER", false},
			{"C_UNLISTED", "U_ALERT", "", false},
		} {
			channel := tc.channel
			id := fmt.Sprintf("shared-%d", i)
			require.NoError(t, socket.WriteJSON(map[string]any{"type": "events_api", "envelope_id": id, "payload": map[string]any{
				"type": "event_callback", "event_id": id, "event": map[string]any{
					"type": "message", "channel": channel, "user": tc.user, "bot_id": tc.bot, "text": "<@U_BOT> hello", "ts": fmt.Sprintf("1.%d", i),
				},
			}}))
			require.Eventually(t, func() bool {
				ref := domain.NewConversationRef(name, channel, fmt.Sprintf("1.%d", i))
				event, err := repository.NewEventRepository(app.db).GetInbound(t.Context(), ref)
				return err == nil && event.Accepted != nil && *event.Accepted == tc.accepted
			}, 5*time.Second, 10*time.Millisecond)
			if tc.accepted {
				post := takePost()
				require.Equal(t, "xoxb-"+name, post.Get("token"))
				require.Equal(t, channel, post.Get("channel"))
			}
		}
	}
	_, err = repository.NewInboxRepository(app.db).Enqueue(t.Context(), domain.NewOwnerNotice("config-order", "ordered notice"))
	require.NoError(t, err)
	post := takePost()
	require.Equal(t, "ordered notice", post.Get("text"))
	require.Equal(t, "D_OWNER", post.Get("channel"))
	require.Equal(t, "xoxb-support", post.Get("token"))
	var sources int
	require.NoError(t, app.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key IN ('support:shared-0','slack:shared-0')").Scan(&sources))
	require.Equal(t, 2, sources)
}
