package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	memoryusecase "github.com/motoki317/noctiluca/pkg/usecase/memory"
	"github.com/stretchr/testify/require"
)

func TestMemoryPromptLoadsOncePerProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	s, err := memory.New(t.Context(), memory.Config{Path: dir})
	require.NoError(t, err)
	for _, name := range []string{"soul.md", "user.md", "rules.md", "legacy.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "owner", name), []byte("Handwritten "+name), 0600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "core.md"), []byte("Initial index"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "topics/secret.md"), []byte("On demand only"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "procedures/heartbeat.md"), []byte("Heartbeat task"), 0600))
	f := setupMemory(t, Config{}, Messaging{Owner: "owner", Rules: map[domain.ConversationRef]domain.AcceptRule{"test:dm": {Users: []string{"owner"}}}}, nil, nil, nil, s)
	f.messaging.in <- message("test:dm", "owner", "first")
	model := take(t, f.model.opened)
	input := take(t, model.inputs)
	require.NotContains(t, input.Text, "Handwritten soul.md")
	for _, name := range []string{"soul.md", "user.md", "rules.md", "legacy.md"} {
		require.Contains(t, model.config.SystemPrompt, "Handwritten "+name)
	}
	require.Contains(t, model.config.SystemPrompt, f.hub.tools["reply"].Description)
	require.NotContains(t, input.Text, f.hub.tools["reply"].Description)
	require.Contains(t, model.config.SystemPrompt, "Initial index")
	require.NotContains(t, model.config.SystemPrompt, "On demand only")
	require.NotContains(t, model.config.SystemPrompt, "Heartbeat task")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "core.md"), []byte("Updated index"), 0600))
	model.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	require.Eventually(t, func() bool {
		records, err := f.events.ListModel(t.Context(), "test:dm", 1)
		return err == nil && len(records) == 1 && records[0].Event.Kind == domain.ModelEventTurnFinished
	}, time.Second, time.Millisecond)
	f.messaging.in <- message("test:dm", "owner", "second")
	input = take(t, model.inputs)
	require.NotContains(t, input.Text, "Updated index")
	require.Contains(t, model.config.SystemPrompt, "Initial index")
	require.NoError(t, model.Close())
	resumed := take(t, f.model.opened)
	require.Contains(t, take(t, resumed.inputs).Text, "previous turn was interrupted: model process exited")
	require.Contains(t, resumed.config.SystemPrompt, "Updated index")
	require.NotContains(t, resumed.config.SystemPrompt, "Initial index")
	require.NotEmpty(t, resumed.config.ResumeToken)
	f.messaging.in <- message("test:dm", "owner", "after restart")
	input = take(t, resumed.inputs)
	require.Contains(t, input.Text, "after restart")
	require.NotContains(t, input.Text, "Initial index")
}

func TestMemoryPromptIncludesNotesAndHook(t *testing.T) {
	prompt := memoryusecase.Prompt(nil, "Use adapter tools", func(_ context.Context, _ domain.ToolSession) (string, error) { return "hook instructions", nil })
	out, err := prompt(t.Context(), domain.ToolSession{})
	require.NoError(t, err)
	require.Contains(t, out, "Use adapter tools")
	require.Contains(t, out, "hook instructions")
	require.NotContains(t, out, "Memory notes:")
}
