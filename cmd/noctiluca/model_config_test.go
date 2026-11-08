package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelConfigSelection(t *testing.T) {
	for _, tc := range []struct {
		body, kind, executable, model, effort string
	}{
		{body: "{}", kind: "claude", executable: "claude"},
		{body: "model: {type: claude}", kind: "claude", executable: "claude"},
		{body: "model: {type: codex}", kind: "codex", executable: "codex"},
		{body: "model: {type: codex, executable: ./custom, name: test-model, reasoning_effort: low}", kind: "codex", executable: "./custom", model: "test-model", effort: "low"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			config, err := loadConfig(path)
			require.NoError(t, err)
			require.Equal(t, tc.kind, config.Model.Type)
			executable := tc.executable
			if executable == "./custom" {
				executable = filepath.Join(filepath.Dir(path), "custom")
			}
			require.Equal(t, executable, config.Model.Executable)
			require.Equal(t, tc.model, config.Model.Name)
			require.Equal(t, tc.effort, config.Model.ReasoningEffort)
		})
	}
	for _, kind := range []string{"unknown", "''", "Codex"} {
		_, err := loadConfig(writeConfig(t, "model: {type: "+kind+"}"))
		require.EqualError(t, err, "model.type must be claude or codex")
	}
}

func TestModelConfigEnvironment(t *testing.T) {
	t.Setenv("NOCTILUCA_MODEL_TYPE", "codex")
	t.Setenv("NOCTILUCA_MODEL_REASONING_EFFORT", "low")
	config, err := loadConfig(writeConfig(t, "{}"))
	require.NoError(t, err)
	require.Equal(t, "codex", config.Model.Type)
	require.Equal(t, "codex", config.Model.Executable)
	require.Equal(t, "low", config.Model.ReasoningEffort)
	t.Setenv("NOCTILUCA_MODEL_EXECUTABLE", "custom-codex")
	config, err = loadConfig(writeConfig(t, "{}"))
	require.NoError(t, err)
	require.Equal(t, "custom-codex", config.Model.Executable)
	t.Setenv("NOCTILUCA_MODEL_TYPE", "invalid")
	_, err = loadConfig(writeConfig(t, "{}"))
	require.EqualError(t, err, "model.type must be claude or codex")
}

func TestClaudeRejectsReasoningEffort(t *testing.T) {
	for _, body := range []string{
		"model: {reasoning_effort: low}",
		"model: {type: claude, reasoning_effort: low}",
		"model: {type: claude, reasoning_effort: ''}",
		"model: {type: claude, reasoning_effort: null}",
		"model: {type: claude, reasoning_effort: {}}",
	} {
		_, err := loadConfig(writeConfig(t, body))
		require.EqualError(t, err, "model.reasoning_effort requires model.type codex")
	}
	for _, value := range []string{"low", ""} {
		t.Setenv("NOCTILUCA_MODEL_REASONING_EFFORT", value)
		_, err := loadConfig(writeConfig(t, "{}"))
		require.EqualError(t, err, "model.reasoning_effort requires model.type codex")
	}
}
