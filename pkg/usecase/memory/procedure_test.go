package memory_test

import (
	"testing"

	memoryusecase "github.com/motoki317/noctiluca/pkg/usecase/memory"
	"github.com/stretchr/testify/require"
)

func TestProcedureRest(t *testing.T) {
	frame, err := memoryusecase.Procedure(t.Context(), "rest")
	require.NoError(t, err)
	require.Contains(t, frame, "If procedures/rest.md exists in memory, read it first. It adds this instance's own steps. Each step says where it fits in this procedure.")
	require.Contains(t, frame, "- Keep in procedures/rest.md only the steps that this procedure lacks. Delete any step there that repeats one here.")
	require.NotContains(t, frame, "<!--")
}

func TestProcedureHeartbeat(t *testing.T) {
	frame, err := memoryusecase.Procedure(t.Context(), "heartbeat")
	require.NoError(t, err)
	require.Contains(t, frame, "If procedures/heartbeat.md exists in memory, read it first. Each of its steps says where it fits in this procedure, or quotes the sentence here that it replaces. Write a step that replaces a sentence here only when the owner asks for it.")
	require.Contains(t, frame, "Delete any step in procedures/heartbeat.md that only repeats a step here.")
	require.NotContains(t, frame, "<!--")
}

func TestProcedureUnknown(t *testing.T) {
	text, err := memoryusecase.Procedure(t.Context(), "unknown")
	require.ErrorContains(t, err, "unknown")
	require.Empty(t, text)
}
