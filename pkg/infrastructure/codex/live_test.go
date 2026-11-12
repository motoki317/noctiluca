//go:build live

package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/usecase/toolhub"
)

// This test spends one subscription turn and checks that Codex leaves config.toml unchanged.
func TestLiveHubTurn(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("live subscription test is disabled in CI")
	}
	workdir := os.Getenv("NOCTILUCA_LIVE_CODEX_WORKDIR")
	if workdir == "" {
		t.Skip("set NOCTILUCA_LIVE_CODEX_WORKDIR to an existing directory without a .codex/ directory")
	}
	_, err := os.Stat(filepath.Join(workdir, ".codex"))
	require.True(t, os.IsNotExist(err), "live workdir must not contain a .codex directory")
	configPath := liveConfigPath(t, workdir)
	before := readLiveConfig(t, configPath)
	t.Logf("config before: exists=%t sha256=%x", before.exists, before.hash)
	t.Cleanup(func() {
		after := readLiveConfig(t, configPath)
		t.Logf("config after: exists=%t sha256=%x", after.exists, after.hash)
		require.Equal(t, before, after, "Codex changed config.toml. The test fails without restoring the file so the changes remain available for inspection.")
	})
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	hub, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	replies := make(chan string, 1)
	require.NoError(t, hub.AddSource("test", []domain.Tool{{Name: "reply", Description: "Send a reply to the current conversation.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`),
		Handler: func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
			var args struct{ Text string }
			if err := json.Unmarshal(input, &args); err != nil {
				return nil, err
			}
			select {
			case replies <- args.Text:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return json.RawMessage(`{"sent":true}`), nil
		}}}))
	hubDone := make(chan error, 1)
	go func() { hubDone <- hub.Start(ctx) }()
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		require.NoError(t, hub.Shutdown(stopCtx))
		require.NoError(t, <-hubDone)
	})
	endpoint, err := hub.OpenSession("live-session")
	require.NoError(t, err)
	s, err := New("", "gpt-6-luna", "low").Open(ctx, domain.ModelSessionConfig{Workdir: workdir, ToolHub: endpoint,
		SystemPrompt: "This is a smoke test. Send replies through the reply tool. Do not read or change files or use other tools."})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NotEmpty(t, s.ResumeToken())
	require.NoError(t, s.Send(ctx, domain.UserInput{Text: "Call the reply tool exactly once with text PONG, then finish."}))
	started, called := false, false
	for {
		select {
		case event, ok := <-s.Events():
			require.True(t, ok)
			require.NoError(t, event.Err)
			switch event.Kind {
			case domain.ModelEventTurnStarted:
				started = true
			case domain.ModelEventToolCall:
				called = event.ToolCall.Name == "mcp__noctiluca__reply" || called
			case domain.ModelEventTurnFinished:
				require.True(t, started)
				require.True(t, called)
				require.True(t, s.Idle())
				select {
				case reply := <-replies:
					require.Equal(t, "PONG", reply)
				default:
					t.Fatal("turn ended without a hub reply")
				}
				require.NoError(t, s.Close())
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func liveConfigPath(t *testing.T, workdir string) string {
	t.Helper()
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		require.NoError(t, err)
		return filepath.Join(userHome, ".codex", "config.toml")
	}
	if !filepath.IsAbs(home) {
		workdir, err := filepath.Abs(workdir)
		require.NoError(t, err)
		// Keep '..' intact until symlinks resolve, as Codex does.
		home = workdir + string(filepath.Separator) + home
	}
	home, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	return filepath.Join(home, "config.toml")
}

type liveConfigState struct {
	exists bool
	hash   [sha256.Size]byte
}

func readLiveConfig(t *testing.T, path string) liveConfigState {
	t.Helper()
	data, err := os.ReadFile(path)
	if !os.IsNotExist(err) {
		require.NoError(t, err)
	}
	return liveConfigState{exists: err == nil, hash: sha256.Sum256(data)}
}
