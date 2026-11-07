package memory_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	prompt, err := memoryusecase.Prompt(store, "Messaging instructions", nil)(t.Context(), domain.ToolSession{})
	require.NoError(t, err)
	lines := []string{
		"Memory notes:",
		"- Read and edit memory in " + strconv.Quote(store.Dir()) + " with your own file tools.",
		"- owner/ holds the owner's handwritten files. Never edit, move, or delete them. Propose fixes to the owner.",
		"- owner/ files and core.md are in this system prompt. Edits take effect in the next model process.",
		"- Keep core.md under 8 KB.",
		"- Everything outside owner/, including core.md, is your own memory.",
		"- Commit each change with a message that says why.",
		"- Commit only paths you changed because other sessions share this repository.",
		"- Retry if git reports a lock.",
	}
	require.True(t, strings.HasPrefix(prompt, strings.Join(lines, "\n")+"\n\n"), prompt)
	require.Contains(t, prompt, "Memory: owner/rules.md\nOwner instructions")
	require.Contains(t, prompt, "Memory: core.md\nMemory index")
	require.Contains(t, prompt, "Messaging tool notes:\nMessaging instructions")
}
