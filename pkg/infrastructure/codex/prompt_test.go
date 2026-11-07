package codex

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func TestPromptRefresh(t *testing.T) {
	for _, token := range []string{"thread-1", "thread-1:" + promptHash(developerInstructions("old prompt"))} {
		t.Run(token, func(t *testing.T) {
			path, dir := fakeServer(t, "normal")
			config := domain.ModelSessionConfig{Workdir: dir, ResumeToken: token, SystemPrompt: "updated memory"}
			s, err := New(path, "", "").Open(t.Context(), config)
			require.NoError(t, err)
			config.ResumeToken = s.ResumeToken()
			require.NoError(t, s.Close())
			require.Equal(t, "thread-1:"+promptHash(developerInstructions(config.SystemPrompt)), config.ResumeToken)
			frames := requests(t, dir)
			require.Len(t, frames, 4)
			require.Equal(t, "thread/resume", frames[2].Method)
			require.Equal(t, "thread/inject_items", frames[3].Method)
			var injected map[string]any
			require.NoError(t, json.Unmarshal(frames[3].Params, &injected))
			require.Equal(t, map[string]any{"threadId": "thread-1", "items": []any{map[string]any{
				"type": "message", "role": "developer", "content": []any{map[string]any{
					"type": "input_text", "text": "updated memory\n\n" + backgroundInstruction,
				}},
			}}}, injected)
			s, err = New(path, "", "").Open(t.Context(), config)
			require.NoError(t, err)
			require.NoError(t, s.Close())
			require.Len(t, requests(t, dir), 7, "unchanged prompt must not be injected again")
		})
	}
}

func TestPromptInjectionUnavailable(t *testing.T) {
	for _, mode := range []string{"no-injection", "unknown-injection"} {
		t.Run(mode, func(t *testing.T) {
			path, dir := fakeServer(t, mode)
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(old) })
			s, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{
				Workdir: dir, ResumeToken: "thread-1:old-hash", SystemPrompt: "new prompt",
			})
			require.NoError(t, err)
			require.NoError(t, s.Close())
			frames := requests(t, dir)
			require.Len(t, frames, 5)
			require.Equal(t, "thread/start", frames[4].Method)
			var params threadParams
			require.NoError(t, json.Unmarshal(frames[4].Params, &params))
			require.Empty(t, params.ThreadID)
			require.False(t, params.ExcludeTurns)
			require.Equal(t, developerInstructions("new prompt"), params.DeveloperInstructions)
			require.Contains(t, logs.String(), "WARN")
			require.Contains(t, logs.String(), "prompt injection is unavailable; starting a fresh session")
			require.Equal(t, "thread-1:"+promptHash(params.DeveloperInstructions), s.ResumeToken())
		})
	}
}

func TestPromptInjectionFailure(t *testing.T) {
	path, dir := fakeServer(t, "injection-error")
	s, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir, ResumeToken: "thread-1"})
	require.ErrorContains(t, err, "refresh codex instructions")
	require.Nil(t, s)
	require.Len(t, requests(t, dir), 4, "invalid input must not silently discard history")
}

func TestPromptFallbackRequiresFreshThreadID(t *testing.T) {
	path, dir := fakeServer(t, "injection-fresh-missing-id")
	s, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir, ResumeToken: "thread-1"})
	require.ErrorContains(t, err, "no thread id")
	require.Nil(t, s, "the old thread must not receive a token claiming updated instructions")
	require.Len(t, requests(t, dir), 5)
}
