// Claude CLI protocol reference: OpenClaw (MIT), https://github.com/openclaw/openclaw.
// Go reimplementation. Provenance and license notice: THIRD_PARTY_NOTICES.md.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdk "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

type runCapture struct {
	PID                   int
	URL, MCPFile, Workdir string
}

type bearerTransport string

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", string(b))
	return http.DefaultTransport.RoundTrip(r)
}

func TestRunCLIHelper(t *testing.T) {
	if os.Getenv("NOCTILUCA_RUN_TEST_BINARY") == "" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("2.1.316 (Claude Code)")
		os.Exit(0)
	}
	capture := runCapture{PID: os.Getpid()}
	capture.Workdir, _ = os.Getwd()
	for i, arg := range args {
		if arg == "--mcp-config" {
			capture.MCPFile = args[i+1]
		}
	}
	data, err := os.ReadFile(capture.MCPFile)
	require.NoError(t, err)
	var config struct {
		Servers map[string]struct {
			URL     string
			Headers map[string]string
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(data, &config))
	endpoint := config.Servers["noctiluca"]
	capture.URL = endpoint.URL
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var frame struct {
			Type    string
			UUID    string
			Message struct{ Content string }
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &frame))
		if frame.Type == "control_request" {
			fmt.Println(`{"type":"control_response","response":{"request_id":"initialize","subtype":"success"}}`)
			fmt.Println(`{"type":"system","subtype":"init","session_id":"run-session"}`)
		}
		if frame.Type != "user" {
			continue
		}
		fmt.Printf("{\"type\":\"command_lifecycle\",\"command_uuid\":%q,\"state\":\"started\"}\n", frame.UUID)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		client := mcp.NewClient(&mcp.Implementation{Name: "fake-claude", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint.URL, HTTPClient: &http.Client{Transport: bearerTransport(endpoint.Headers["Authorization"])}}, nil)
		require.NoError(t, err)
		listed, err := session.ListTools(ctx, nil)
		require.NoError(t, err)
		require.NotEmpty(t, listed.Tools)
		for _, tool := range listed.Tools {
			require.Equal(t, mcp.Meta{"anthropic/alwaysLoad": true}, tool.Meta, tool.Name)
		}
		_, refs, ok := strings.Cut(frame.Message.Content, "\nrefs: conversation=")
		require.True(t, ok)
		line, _, _ := strings.Cut(refs, "\n")
		target, _, _ := strings.Cut(line, " thread=")
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "reply", Arguments: map[string]any{"target": target, "text": "**Hello** from the hub"}})
		require.NoError(t, err)
		require.False(t, result.IsError, "%+v", result.Content)
		require.NoError(t, session.Close())
		cancel()
		fmt.Println(`{"type":"result","subtype":"success","session_id":"run-session"}`)
		data, err := json.Marshal(capture)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("NOCTILUCA_RUN_TEST_DIR"), "capture.json"), data, 0600))
	}
	os.Exit(0)
}

func newRunCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binary, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("NOCTILUCA_RUN_TEST_BINARY", binary)
	t.Setenv("NOCTILUCA_RUN_TEST_DIR", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexec \"$NOCTILUCA_RUN_TEST_BINARY\" -test.run=TestRunCLIHelper -- \"$@\"\n"), 0700))
	return dir
}

func TestRunAssemblyReplyAndShutdown(t *testing.T) {
	dir := newRunCLI(t)
	t.Setenv("TEST_APP_TOKEN", "xapp-fake")
	t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
	api := newRunSlack(t, nil)
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(slackEntry+`    slack:
      app_token: {env: TEST_APP_TOKEN}
      bot_token: {env: TEST_BOT_TOKEN}
model: {executable: ./claude}
heartbeat: {enabled: true}
cron:
  - {name: daily, cron: '0 9 * * *', tz: UTC, prompt: Check tasks}
`), 0600))
	c, err := loadConfig(path)
	require.NoError(t, err)
	var logs bytes.Buffer
	app, err := newApplication(t.Context(), c, slog.New(slog.NewTextHandler(&logs, nil)), sdk.OptionAPIURL(api.URL+"/"))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		stop, finish := context.WithTimeout(context.Background(), 5*time.Second)
		defer finish()
		require.NoError(t, app.Shutdown(stop))
	})
	var socket *websocket.Conn
	select {
	case socket = <-api.connections:
	case <-time.After(5 * time.Second):
		t.Fatal("socket startup timed out")
	}
	require.NoError(t, socket.WriteJSON(map[string]any{
		"type": "events_api", "envelope_id": "input", "payload": map[string]any{
			"type": "event_callback", "event_id": "input", "event": map[string]any{
				"type": "message", "channel": "D_DIRECT", "channel_type": "im", "user": "U_OWNER", "text": "hello", "ts": "1.2",
			},
		},
	}))
	select {
	case ack := <-api.acks:
		require.Equal(t, "input", ack)
		var events, inbox int
		require.NoError(t, app.db.QueryRow(`SELECT
			(SELECT count(*) FROM events WHERE message_ref='slack:D_DIRECT:1.2'),
			(SELECT count(*) FROM inbox WHERE source_key='slack:m:D_DIRECT:1.2')`).Scan(&events, &inbox))
		require.Equal(t, 1, events, "event must persist before acknowledgement")
		require.Equal(t, 1, inbox, "input must persist before acknowledgement")
	case <-time.After(5 * time.Second):
		t.Fatal("missing durable acknowledgement")
	}
	select {
	case posted := <-api.posts:
		require.Equal(t, "D_DIRECT", posted.Get("channel"))
		require.Equal(t, "*Hello* from the hub", posted.Get("text"))
	case err := <-done:
		t.Fatalf("run stopped: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	var capture runCapture
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "capture.json"))
		return err == nil && json.Unmarshal(data, &capture) == nil
	}, time.Second, time.Millisecond)
	require.Equal(t, c.Model.Workdir, capture.Workdir)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	require.Eventually(t, func() bool {
		return socket.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)) != nil
	}, time.Second, time.Millisecond, "socket still open")
	require.Error(t, app.db.Ping())
	require.NoError(t, app.Shutdown(t.Context()))
	require.NotContains(t, logs.String(), "xoxb-fake")
	require.NotContains(t, logs.String(), "xapp-fake")
	require.NoFileExists(t, capture.MCPFile)
	endpoint, err := url.Parse(capture.URL)
	require.NoError(t, err)
	conn, err := net.DialTimeout("tcp", endpoint.Host, 100*time.Millisecond)
	if conn != nil {
		conn.Close()
	}
	require.Error(t, err)
	require.Positive(t, capture.PID)
	require.ErrorIs(t, syscall.Kill(capture.PID, 0), syscall.ESRCH)

	db, err := repository.New(t.Context(), c.Database)
	require.NoError(t, err)
	defer db.Close()
	outbound, err := repository.NewEventRepository(db).ListOutbound(t.Context(), "slack:D_DIRECT:", 10)
	require.NoError(t, err)
	require.Len(t, outbound, 1)
	require.Equal(t, "**Hello** from the hub", outbound[0].Message.Text)
	session, err := repository.NewSessionRepository(db).Get(t.Context(), "slack:D_DIRECT:")
	require.NoError(t, err)
	require.Equal(t, "run-session", session.ResumeToken)
	schedules, err := repository.NewScheduleRepository(db).List(t.Context())
	require.NoError(t, err)
	require.Len(t, schedules, 2)
	for _, schedule := range schedules {
		require.Equal(t, c.Model.Workdir, schedule.Workdir)
	}
}

type blockingStartupTransport struct{ entered chan struct{} }

func (b blockingStartupTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	close(b.entered)
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestRunCancellationDuringStartup(t *testing.T) {
	for _, groups := range []bool{false, true} {
		t.Run(fmt.Sprint(groups), func(t *testing.T) {
			t.Setenv("TEST_APP_TOKEN", "xapp-fake")
			t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
			yaml := slackEntry + "    slack:\n      app_token: {env: TEST_APP_TOKEN}\n      bot_token: {env: TEST_BOT_TOKEN}\n"
			if groups {
				yaml += "    trust: {groups: [S_GROUP]}\n"
			}
			c, err := loadConfig(writeConfig(t, yaml))
			require.NoError(t, err)
			entered := make(chan struct{})
			app, err := newApplication(t.Context(), c, nil, sdk.OptionHTTPClient(&http.Client{Transport: blockingStartupTransport{entered}}))
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- app.Start(ctx) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("startup did not reach HTTP request")
			}
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("startup cancellation blocked")
			}
			require.Error(t, app.db.Ping())
		})
	}
}
