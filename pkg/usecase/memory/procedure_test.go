package memory_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	memoryusecase "github.com/motoki317/noctiluca/pkg/usecase/memory"
	"github.com/stretchr/testify/require"
)

func TestProcedureHeartbeat(t *testing.T) {
	store, err := memory.New(t.Context(), memory.Config{Path: t.TempDir()})
	require.NoError(t, err)
	procedure := memoryusecase.Procedure(store)

	text, err := procedure(t.Context(), "heartbeat")
	require.NoError(t, err)
	require.Empty(t, text)

	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "procedures/heartbeat.md"), []byte(" \nCheck the task list.\n\t"), 0600))
	text, err = procedure(t.Context(), "heartbeat")
	require.NoError(t, err)
	require.Equal(t, "Check the task list.", text)
}
