package memory_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	memoryusecase "github.com/motoki317/noctiluca/pkg/usecase/memory"
	"github.com/stretchr/testify/require"
)

func TestPromptMemoryNotes(t *testing.T) {
	t.Chdir(t.TempDir())
	store, err := memory.New(t.Context(), memory.Config{Path: "memory with spaces"})
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(store.Dir()))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "owner/rules.md"), []byte("Owner instructions"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "core.md"), []byte("Memory index"), 0600))
	prompt, err := memoryusecase.Prompt(store, "Messaging instructions", func(context.Context, domain.ToolSession) (string, error) { return "Hook instructions", nil })(t.Context(), domain.ToolSession{})
	require.NoError(t, err)
	require.Contains(t, prompt, strconv.Quote(store.Dir()))
	require.Contains(t, prompt, "Memory: owner/rules.md\nOwner instructions")
	require.Contains(t, prompt, "Memory: core.md\nMemory index")
	require.Contains(t, prompt, "Messaging tool notes:\nMessaging instructions")
	require.Contains(t, prompt, "Hook instructions")
	prompt, err = memoryusecase.Prompt(nil, "", nil)(t.Context(), domain.ToolSession{})
	require.NoError(t, err)
	require.NotContains(t, prompt, "Memory notes:")
}

func TestPromptRemoteBackup(t *testing.T) {
	const bullet = "- The memory repository pushes to a remote backup that nothing else writes. Push after each commit. When a push is rejected, run `git push --force-with-lease`; never pull. Tell the owner when a push fails for another reason.\n"
	store, err := memory.New(t.Context(), memory.Config{Path: t.TempDir()})
	require.NoError(t, err)
	assemble := memoryusecase.Prompt(store, "", nil)
	prompt, err := assemble(t.Context(), domain.ToolSession{})
	require.NoError(t, err)
	require.NotContains(t, prompt, bullet)
	cmd := exec.Command("git", "-C", store.Dir(), "remote", "add", "backup", t.TempDir())
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	prompt, err = assemble(t.Context(), domain.ToolSession{})
	require.NoError(t, err)
	require.Contains(t, prompt, bullet)
}
