package codex

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func TestPromptRefresh(t *testing.T) {
	const systemPrompt = "updated memory"
	token := "thread-1:" + promptHash(developerInstructions(systemPrompt, []string{"watch"}))
	path, dir := fakeServer(t, "normal")
	config := domain.ModelSessionConfig{Workdir: dir, ResumeToken: token, SystemPrompt: systemPrompt}
	wantPrompt := developerInstructions(systemPrompt, nil)
	require.Contains(t, wantPrompt, "Each brief must say that the subagent never posts. Continue a subagent with send_input.")
	s, err := New(path, "", "").Open(t.Context(), config)
	require.NoError(t, err)
	config.ResumeToken = s.ResumeToken()
	require.NoError(t, s.Close())
	require.Equal(t, "thread-1:"+promptHash(wantPrompt), config.ResumeToken)
	frames := requests(t, dir)
	require.Len(t, frames, 4)
	require.Equal(t, "thread/resume", frames[2].Method)
	require.Equal(t, "thread/inject_items", frames[3].Method)
	var injected map[string]any
	require.NoError(t, json.Unmarshal(frames[3].Params, &injected))
	require.Equal(t, map[string]any{"threadId": "thread-1", "items": []any{map[string]any{
		"type": "message", "role": "developer", "content": []any{map[string]any{
			"type": "input_text", "text": wantPrompt,
		}},
	}}}, injected)
	s, err = New(path, "", "").Open(t.Context(), config)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.Len(t, requests(t, dir), 7, "unchanged prompt must not be injected again")
}

func TestPromptInjectionUnavailable(t *testing.T) {
	for _, mode := range []string{"no-injection", "unknown-injection"} {
		t.Run(mode, func(t *testing.T) {
			path, dir := fakeServer(t, mode)
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
			require.Equal(t, developerInstructions("new prompt", nil), params.DeveloperInstructions)
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
