package memory_test

import (
	"context"
	"os"
	"os/exec"
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
	prompt, err := memoryusecase.Prompt(store, "Messaging instructions", func(context.Context, domain.ToolSession) (string, error) { return "Hook instructions", nil })(t.Context(), domain.ToolSession{})
	require.NoError(t, err)
	require.Contains(t, prompt, strconv.Quote(store.Dir()))
	require.Contains(t, prompt, "Memory: owner/rules.md\nOwner instructions")
	require.Contains(t, prompt, "Memory: core.md\nMemory index")
	require.Contains(t, prompt, "Session and messaging notes:\nMessaging instructions")
	require.Contains(t, prompt, "Hook instructions")
	require.Contains(t, prompt, "Subagents and background commands also commit here. Commit only the paths you changed.")
	require.Contains(t, prompt, "stage only your own hunks, and commit only what you staged. Retry if git reports a lock.")
	require.Contains(t, prompt, "When you take on a task that may outlive this turn, add it at once.")
	require.Contains(t, prompt, "Make each entry a pointer of about ten words: links to its sources, its done condition, and its next check.")
	require.Contains(t, prompt, "For a conversation, use the IDs that its inputs show.")
	require.Contains(t, prompt, "The next check is a time, or a message that will reach you.")
	require.Contains(t, prompt, "Where a conversation, a file, or a ticket holds the details, link to it instead of copying them.")
	require.Contains(t, prompt, "The heartbeat and rest schedules send procedures built into noctiluca. procedures/heartbeat.md and procedures/rest.md hold this instance's own steps for them.")
	require.Contains(t, prompt, "Follow the rest procedure only when the nightly schedule sends it. Call the rest tool only as its last step.")
	require.NotContains(t, prompt, "when you judge that your context needs a fresh start")
	prompt, err = memoryusecase.Prompt(nil, "", nil)(t.Context(), domain.ToolSession{})
	require.NoError(t, err)
	require.NotContains(t, prompt, "Memory notes:")
}

func TestPromptRemoteBackup(t *testing.T) {
	const bullet = "- The memory repository pushes to a remote backup that nothing else writes. Push after each commit. If a push is rejected, run `git push --force-with-lease`. Never pull. If a push fails for another reason, tell the owner.\n"
	store, err := memory.New(t.Context(), memory.Config{Path: t.TempDir()})
	require.NoError(t, err)
	assemble := memoryusecase.Prompt(store, "", nil)
	prompt, err := assemble(t.Context(), domain.ToolSession{})
	require.NoError(t, err)
	require.NotContains(t, prompt, bullet)
	cmd := exec.Command("git", "-C", store.Dir(), "remote", "add", "backup", t.TempDir())
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "GIT_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	prompt, err = assemble(t.Context(), domain.ToolSession{})
	require.NoError(t, err)
	require.Contains(t, prompt, bullet)
}
