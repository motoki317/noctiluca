package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdk "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func TestCodexRunHelper(t *testing.T) {
	if os.Getenv("NOCTILUCA_CODEX_RUN_BINARY") == "" {
		return
	}
	require.Contains(t, os.Args, "app-server")
	var endpoint struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"http_headers"`
	}
	emit := func(value any) { require.NoError(t, json.NewEncoder(os.Stdout).Encode(value)) }
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var request struct {
			ID     json.RawMessage
			Method string
			Params struct {
				Model  string
				Config map[string]json.RawMessage
				Input  []struct{ Text string }
			}
		}
		require.NoError(t, json.Unmarshal(scan.Bytes(), &request))
		respond := func(result any) { emit(map[string]any{"id": request.ID, "result": result}) }
		switch request.Method {
		case "initialize":
			respond(map[string]any{})
		case "thread/start":
			require.Equal(t, "test-model", request.Params.Model)
			require.JSONEq(t, `"low"`, string(request.Params.Config["model_reasoning_effort"]))
			require.NoError(t, json.Unmarshal(request.Params.Config["mcp_servers.noctiluca"], &endpoint))
			respond(map[string]any{"thread": map[string]string{"id": "codex-thread"}})
		case "turn/start":
			respond(map[string]any{"turn": map[string]string{"id": "codex-turn"}})
			emit(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "codex-thread", "turn": map[string]string{"id": "codex-turn", "status": "inProgress"}}})
			if os.Getenv("NOCTILUCA_CODEX_RUN_HANG") == "1" {
				continue
			}
			_, targets, ok := strings.Cut(request.Params.Input[0].Text, "reply → ")
			require.True(t, ok)
			target, _, _ := strings.Cut(targets, " | work → ")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			client := mcp.NewClient(&mcp.Implementation{Name: "fake-codex", Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint.URL,
				HTTPClient: &http.Client{Transport: bearerTransport(endpoint.Headers["Authorization"])}}, nil)
			require.NoError(t, err)
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "reply", Arguments: map[string]any{"target": target, "text": "PONG"}})
			require.NoError(t, err)
			require.False(t, result.IsError)
			require.NoError(t, session.Close())
			cancel()
			emit(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "codex-thread", "turn": map[string]string{"id": "codex-turn", "status": "completed"}}})
		case "turn/interrupt":
			require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("NOCTILUCA_CODEX_RUN_DIR"), "interrupted"), []byte("interrupted"), 0600))
			respond(map[string]any{})
		}
	}
	os.Exit(0)
}

func TestRunCodexReplyAndTurnTimeout(t *testing.T) {
	for _, hang := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%t", hang), func(t *testing.T) {
			dir := t.TempDir()
			binary, err := os.Executable()
			require.NoError(t, err)
			t.Setenv("NOCTILUCA_CODEX_RUN_BINARY", binary)
			t.Setenv("NOCTILUCA_CODEX_RUN_DIR", dir)
			if hang {
				t.Setenv("NOCTILUCA_CODEX_RUN_HANG", "1")
			}
			t.Setenv("TEST_APP_TOKEN", "xapp-fake")
			t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nexec \"$NOCTILUCA_CODEX_RUN_BINARY\" -test.run=TestCodexRunHelper -- \"$@\"\n"), 0700))
			path := filepath.Join(dir, "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(`messaging:
  - name: slack
    type: slack
    owner: U_OWNER
    slack:
      app_token: {env: TEST_APP_TOKEN}
      bot_token: {env: TEST_BOT_TOKEN}
model: {type: codex, executable: ./codex, name: test-model, reasoning_effort: low, turn_timeout: 1s}
heartbeat: {enabled: false}
`), 0600))
			config, err := loadConfig(path)
			require.NoError(t, err)
			api := newRunSlack(t, nil)
			app, err := newApplication(t.Context(), config, nil, sdk.OptionAPIURL(api.URL+"/"))
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
					t.Error("Codex assembly shutdown timed out")
				}
			}()
			select {
			case socket := <-api.connections:
				require.NoError(t, socket.WriteJSON(map[string]any{"type": "events_api", "envelope_id": "message", "payload": map[string]any{
					"type": "event_callback", "event_id": "message", "event": map[string]any{"type": "message", "channel": "D_OWNER", "user": "U_OWNER", "text": "hello", "ts": "1.2"},
				}}))
			case <-time.After(5 * time.Second):
				t.Fatal("socket startup timed out")
			}
			if hang {
				require.Eventually(t, func() bool {
					_, err := os.Stat(filepath.Join(dir, "interrupted"))
					return err == nil
				}, 5*time.Second, 10*time.Millisecond, "turn_timeout did not interrupt Codex")
				return
			}
			select {
			case post := <-api.posts:
				require.Equal(t, "D_OWNER", post.Get("channel"))
				require.Equal(t, "PONG", post.Get("text"))
			case <-time.After(5 * time.Second):
				t.Fatal("Codex reply timed out")
			}
			require.Eventually(t, func() bool {
				saved, err := repository.NewSessionRepository(app.db).Get(t.Context(), domain.SessionKey("slack:D_OWNER:"))
				return err == nil && saved.ResumeToken == "codex-thread" && saved.Turn == nil
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
}
