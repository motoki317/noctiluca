package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelConfigSelection(t *testing.T) {
	for _, tc := range []struct {
		body, kind string
	}{
		{body: "{}", kind: "claude"},
		{body: "model: {type: codex}", kind: "codex"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			config, err := loadConfig(path)
			require.NoError(t, err)
			require.Equal(t, tc.kind, config.Model.Type)
			require.Equal(t, tc.kind, config.Model.Executable)
		})
	}
	_, err := loadConfig(writeConfig(t, "model: {type: unknown}"))
	require.ErrorContains(t, err, "model.type must be claude or codex")
}

func TestModelConfigEnvironment(t *testing.T) {
	t.Setenv("NOCTILUCA_MODEL_TYPE", "codex")
	t.Setenv("NOCTILUCA_MODEL_REASONING_EFFORT", "low")
	config, err := loadConfig(writeConfig(t, "{}"))
	require.NoError(t, err)
	require.Equal(t, "codex", config.Model.Type)
	require.Equal(t, "codex", config.Model.Executable)
	require.Equal(t, "low", config.Model.ReasoningEffort)
}

func TestClaudeRejectsReasoningEffort(t *testing.T) {
	for _, body := range []string{
		"model: {reasoning_effort: low}",
		"model: {type: claude, reasoning_effort: null}",
		"model: {type: claude, reasoning_effort: {}}",
	} {
		_, err := loadConfig(writeConfig(t, body))
		require.EqualError(t, err, "model.reasoning_effort requires model.type codex")
	}
	t.Setenv("NOCTILUCA_MODEL_REASONING_EFFORT", "")
	_, err := loadConfig(writeConfig(t, "{}"))
	require.EqualError(t, err, "model.reasoning_effort requires model.type codex")
}
