package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	sdk "github.com/traPtitech/go-traq"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func TestRunTraQCLIHelper(t *testing.T) {
	if os.Getenv("NOCTILUCA_TRAQ_HELPER") == "" {
		return
	}
	for _, arg := range os.Args {
		if arg == "--version" {
			fmt.Println("2.1.316 (Claude Code)")
			os.Exit(0)
		}
	}
	var config struct {
		Servers map[string]struct {
			URL     string
			Headers map[string]string
		} `json:"mcpServers"`
	}
	for i, arg := range os.Args {
		if arg == "--mcp-config" {
			raw, err := os.ReadFile(os.Args[i+1])
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &config))
		}
		if arg == "--append-system-prompt-file" {
			raw, err := os.ReadFile(os.Args[i+1])
			require.NoError(t, err)
			require.Contains(t, string(raw), "team_read_message")
			require.Contains(t, string(raw), "quote_message_id")
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var frame struct {
			Type    string
			Message struct{ Content string }
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &frame))
		if frame.Type == "control_request" {
			fmt.Println(`{"type":"control_response","response":{"request_id":"initialize","subtype":"success"}}`)
			fmt.Println(`{"type":"system","subtype":"init","session_id":"traq-test"}`)
		}
		if frame.Type != "user" {
			continue
		}
		_, targets, ok := strings.Cut(frame.Message.Content, "work → ")
		require.True(t, ok)
		target, _, _ := strings.Cut(targets, "\n")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		endpoint := config.Servers["noctiluca"]
		client := mcp.NewClient(&mcp.Implementation{Name: "test-model", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint.URL, HTTPClient: &http.Client{Transport: bearerTransport(endpoint.Headers["Authorization"])}}, nil)
		require.NoError(t, err)
		listed, err := session.ListTools(ctx, nil)
		require.NoError(t, err)
		names := make(map[string]bool)
		for _, tool := range listed.Tools {
			names[tool.Name] = true
		}
		for _, name := range []string{"send", "read_history", "read_message", "search", "download_file", "upload_file", "react"} {
			require.True(t, names["team_"+name], name)
		}
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "reply", Arguments: map[string]any{"target": target, "text": "work result"}})
		require.NoError(t, err)
		require.False(t, result.IsError, "%+v", result.Content)
		require.NoError(t, session.Close())
		cancel()
		fmt.Println(`{"type":"result","subtype":"success","session_id":"traq-test"}`)
	}
	os.Exit(0)
}

func TestRunTraQAssemblyRepliesAndBackfill(t *testing.T) {
	const ownerID = "00000000-0000-4000-8000-000000000001"
	const channelID = "00000000-0000-4000-8000-000000000002"
	const groupID = "00000000-0000-4000-8000-000000000003"
	dir := t.TempDir()
	binary, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("NOCTILUCA_TRAQ_HELPER", binary)
	t.Setenv("TEST_TRAQ_TOKEN", "fake-traq-token")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "model"), []byte("#!/bin/sh\nexec \"$NOCTILUCA_TRAQ_HELPER\" -test.run=TestRunTraQCLIHelper -- \"$@\"\n"), 0700))
	at := time.Now().Add(-time.Minute).UTC()
	type posted struct{ channel, text string }
	posts := make(chan posted, 4)
	socketReady := make(chan *websocket.Conn, 1)
	var channelReads, dmReads, groupReads atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer fake-traq-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		write := func(value any) { require.NoError(t, json.NewEncoder(w).Encode(value)) }
		switch r.URL.Path {
		case "/api/v3/users/me":
			write(sdk.MyUserDetail{Id: "bot"})
		case "/api/v3/channels/" + channelID + "/bots":
			write([]sdk.BotUser{{Id: "agent", BotUserId: "bot"}})
		case "/api/v3/users/" + ownerID:
			write(sdk.UserDetail{Id: ownerID, DisplayName: "Owner"})
		case "/api/v3/users/" + ownerID + "/dm-channel":
			write(sdk.DMChannel{Id: "dm", UserId: ownerID})
		case "/api/v3/groups/" + groupID + "/members":
			groupReads.Add(1)
			write([]sdk.UserGroupMember{{Id: ownerID}})
		case "/api/v3/channels/" + channelID:
			write(sdk.Channel{Id: channelID, Name: "test"})
		case "/api/v3/channels/" + channelID + "/messages", "/api/v3/channels/dm/messages":
			channel := channelID
			if strings.Contains(r.URL.Path, "/dm/") {
				channel = "dm"
			}
			if r.Method == http.MethodPost {
				var message sdk.PostMessageRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&message))
				require.True(t, message.GetEmbed())
				posts <- posted{channel: channel, text: message.Content}
				write(sdk.Message{Id: "sent-" + channel, ChannelId: channel, UserId: "bot", Content: message.Content, CreatedAt: time.Now(), UpdatedAt: time.Now(), Stamps: []sdk.MessageStamp{}})
				return
			}
			if channel == channelID {
				channelReads.Add(1)
				write([]sdk.Message{{Id: "request", ChannelId: channelID, UserId: ownerID, Content: `!{"type":"user","id":"bot"} do work`, CreatedAt: at, UpdatedAt: at, Stamps: []sdk.MessageStamp{}}})
			} else {
				dmReads.Add(1)
				write([]sdk.Message{})
			}
		case "/api/v3/bots/ws":
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			require.NoError(t, err)
			defer conn.Close()
			socketReady <- conn
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		default:
			t.Errorf("unexpected API %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer api.Close()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(`messaging:
  - name: team
    type: traq
    owner: %s
    traq: {origin: %q, access_token: {env: TEST_TRAQ_TOKEN}}
    conversations:
      rules: [{id: %s, groups: [%s], require_mention: true}]
model: {executable: ./model}
heartbeat: {enabled: false}
consolidate: {enabled: false}
`, ownerID, api.URL, channelID, groupID)), 0600))
	config, err := loadConfig(path)
	require.NoError(t, err)
	app, err := newApplication(t.Context(), config, nil)
	require.NoError(t, err)
	storedAccepted := true
	require.NoError(t, repository.NewEventRepository(app.db).SaveInbound(t.Context(), domain.InboundEvent{
		Kind: domain.InboundEventMessage, Conversation: domain.NewConversationRef("team", channelID, ""), Accepted: &storedAccepted,
		Sender: domain.Sender{ID: ownerID, Kind: domain.SenderHuman}, Text: "stored message", ReceivedAt: at.Add(-time.Hour),
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.Start(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("traQ shutdown timed out")
		}
	}()
	takePost := func() posted {
		t.Helper()
		select {
		case post := <-posts:
			return post
		case <-time.After(10 * time.Second):
			t.Fatal("reply timed out")
			return posted{}
		}
	}
	post := takePost()
	require.Equal(t, channelID, post.channel)
	require.Equal(t, "work result\n"+api.URL+"/messages/request", post.text)
	var conn *websocket.Conn
	select {
	case conn = <-socketReady:
	case <-time.After(time.Second):
		t.Fatal("WebSocket not ready")
	}
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "DIRECT_MESSAGE_CREATED", "body": map[string]any{"eventTime": at, "message": map[string]any{"id": "dm-request", "channelId": "dm", "user": map[string]any{"id": ownerID, "displayName": "Owner", "bot": false}, "text": "do work", "createdAt": at, "updatedAt": at}}}))
	post = takePost()
	require.Equal(t, "dm", post.channel)
	require.Equal(t, "work result", post.text)
	require.EqualValues(t, 1, channelReads.Load())
	require.EqualValues(t, 1, dmReads.Load())
	require.GreaterOrEqual(t, groupReads.Load(), int32(1))
	require.Eventually(t, func() bool {
		var count int
		err := app.db.QueryRow("SELECT count(*) FROM sends").Scan(&count)
		return err == nil && count == 2
	}, time.Second, 10*time.Millisecond)
	var accepted int
	require.NoError(t, app.db.QueryRow("SELECT count(*) FROM events WHERE accepted=1").Scan(&accepted))
	require.Equal(t, 3, accepted)
}
