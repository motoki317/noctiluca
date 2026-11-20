package memory_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	memoryusecase "github.com/motoki317/noctiluca/pkg/usecase/memory"
	"github.com/stretchr/testify/require"
)

func TestProcedureRest(t *testing.T) {
	store, err := memory.New(t.Context(), memory.Config{Path: t.TempDir()})
	require.NoError(t, err)
	procedure := memoryusecase.Procedure(store)

	frame, err := procedure(t.Context(), "rest")
	require.NoError(t, err)
	require.Contains(t, frame, "If procedures/rest.md exists in memory, read it first. It adds this instance's own steps. Each step says where it fits in this procedure.")
	require.Contains(t, frame, "- Keep in procedures/rest.md only the steps that this procedure lacks. Delete any step there that repeats one here.")
	require.NotContains(t, frame, "<!--")

	const instanceStep = "Add the line REST_MARKER to short-term.md."
	require.NoError(t, os.Mkdir(filepath.Join(store.Dir(), "procedures"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "procedures/rest.md"), []byte(instanceStep), 0600))
	text, err := procedure(t.Context(), "rest")
	require.NoError(t, err)
	require.Equal(t, frame, text)
	require.NotContains(t, text, instanceStep)
}

func TestProcedureHeartbeat(t *testing.T) {
	store, err := memory.New(t.Context(), memory.Config{Path: t.TempDir()})
	require.NoError(t, err)
	procedure := memoryusecase.Procedure(store)

	text, err := procedure(t.Context(), "heartbeat")
	require.NoError(t, err)
	require.Empty(t, text)

	require.NoError(t, os.Mkdir(filepath.Join(store.Dir(), "procedures"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "procedures/heartbeat.md"), []byte(" \nCheck the task list.\n\t"), 0600))
	text, err = procedure(t.Context(), "heartbeat")
	require.NoError(t, err)
	require.Equal(t, "Check the task list.", text)
}
