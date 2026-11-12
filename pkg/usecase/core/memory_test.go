package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	"github.com/stretchr/testify/require"
)

func TestMemoryPromptLoadsOncePerProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	s, err := memory.New(t.Context(), memory.Config{Path: dir})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "owner/rules.md"), []byte("Owner instructions"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "core.md"), []byte("Initial index"), 0600))
	f := setup(t, Config{}, []Messaging{{Owner: "owner", Rules: map[domain.ConversationRef]domain.AcceptRule{"test:dm": {Users: []string{"owner"}}}}}, fixtureOptions{memory: s})
	f.messaging.in <- message("test:dm", "owner", "first")
	model := take(t, f.model.opened)
	input := take(t, model.inputs)
	require.NotContains(t, input.Text, "Owner instructions")
	require.Contains(t, model.config.SystemPrompt, "Owner instructions")
	require.Contains(t, model.config.SystemPrompt, f.hub.tools["reply"].Description)
	require.NotContains(t, input.Text, f.hub.tools["reply"].Description)
	require.Contains(t, model.config.SystemPrompt, "Initial index")
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
	take(t, resumed.inputs)
	require.Contains(t, resumed.config.SystemPrompt, "Updated index")
	require.NotContains(t, resumed.config.SystemPrompt, "Initial index")
	f.messaging.in <- message("test:dm", "owner", "after restart")
	input = take(t, resumed.inputs)
	require.Contains(t, input.Text, "after restart")
}
