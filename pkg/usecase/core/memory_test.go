package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	memoryusecase "github.com/motoki317/noctiluca/pkg/usecase/memory"
	"github.com/stretchr/testify/require"
)

func TestMemoryPromptLoadsOncePerProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	s, err := memory.New(t.Context(), memory.Config{Path: dir})
	require.NoError(t, err)
	owner := domain.WithToolSession(t.Context(), domain.ToolSession{Key: "dm", Actor: domain.ActorOwner})
	for _, name := range []string{"soul.md", "user.md", "rules.md", "legacy.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "owner", name), []byte("Handwritten "+name), 0600))
	}
	require.NoError(t, s.Write(owner, "core.md", "Initial index", "Set core"))
	require.NoError(t, s.Write(owner, "topics/secret.md", "On demand only", "Record topic"))
	require.NoError(t, s.Write(owner, "procedures/heartbeat.md", "Heartbeat task", "Set heartbeat"))
	f := setupMemory(t, Config{Owner: "owner", Rules: map[domain.ConversationRef]domain.AcceptRule{"dm": {Users: []string{"owner"}}}}, nil, nil, nil, s)
	f.messaging.in <- message("dm", "owner", "first")
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
	write := f.hub.tools["memory_write"]
	_, err = write.Handler(owner, json.RawMessage(`{"path":"core.md","content":"Updated index","message":"Update core"}`))
	require.NoError(t, err)
	model.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.messaging.in <- message("dm", "owner", "second")
	input = take(t, model.inputs)
	require.NotContains(t, input.Text, "Updated index")
	require.Contains(t, model.config.SystemPrompt, "Initial index")
	require.NoError(t, model.Close())
	f.messaging.in <- message("dm", "owner", "after restart")
	resumed := take(t, f.model.opened)
	require.Contains(t, resumed.config.SystemPrompt, "Updated index")
	require.NotContains(t, resumed.config.SystemPrompt, "Initial index")
	require.NotEmpty(t, resumed.config.ResumeToken)
	require.NotContains(t, input.Text, "Initial index")
	result, err := f.hub.tools["memory_read"].Handler(owner, json.RawMessage(`{"path":"procedures/heartbeat.md"}`))
	require.NoError(t, err)
	require.Contains(t, string(result), "Heartbeat task")
	result, err = f.hub.tools["memory_search"].Handler(owner, json.RawMessage(`{"query":"On demand"}`))
	require.NoError(t, err)
	require.Contains(t, string(result), "topics/secret.md")
	member := domain.WithToolSession(t.Context(), domain.ToolSession{Key: "room", Actor: domain.ActorMember})
	_, err = write.Handler(member, json.RawMessage(`{"path":"owner/persona.md","content":"forged","message":"Bypass owner"}`))
	require.ErrorContains(t, err, "owner/ is read-only")
	args, err := json.Marshal(map[string]string{"path": "core.md", "content": strings.Repeat("x", 8193), "message": "Too large"})
	require.NoError(t, err)
	_, err = write.Handler(owner, args)
	require.ErrorContains(t, err, "merge or delete")
}

func TestMemoryPromptIncludesNotesAndHook(t *testing.T) {
	prompt := memoryusecase.Prompt(nil, "Use adapter tools", func(_ context.Context, _ domain.ToolSession) (string, error) { return "hook instructions", nil })
	out, err := prompt(t.Context(), domain.ToolSession{Actor: domain.ActorMember})
	require.NoError(t, err)
	require.Contains(t, out, "Use adapter tools")
	require.Contains(t, out, "hook instructions")
}

func TestMemoryHistoryTools(t *testing.T) {
	s, err := memory.New(t.Context(), memory.Config{Path: filepath.Join(t.TempDir(), "memory"), Limits: domain.MemoryLimits{TotalBytes: 32}})
	require.NoError(t, err)
	tools := map[string]domain.Tool{}
	for _, tool := range memoryusecase.Tools(s) {
		tools[tool.Name] = tool
	}
	owner := domain.WithToolSession(t.Context(), domain.ToolSession{Key: "task:consolidate", Actor: domain.ActorOwner})
	call := func(name, input string) json.RawMessage {
		t.Helper()
		result, err := tools[name].Handler(owner, json.RawMessage(input))
		require.NoError(t, err)
		return result
	}
	result := call("memory_write", `{"path":"core.md","content":"candidate","message":"Test one delta"}`)
	var write struct {
		Revision string `json:"revision"`
	}
	require.NoError(t, json.Unmarshal(result, &write))
	require.NotEmpty(t, write.Revision)
	var sizes domain.MemorySizes
	require.NoError(t, json.Unmarshal(call("memory_sizes", `{}`), &sizes))
	require.EqualValues(t, 9, sizes.CoreBytes)
	require.EqualValues(t, 32, sizes.Limits.TotalBytes)
	require.Equal(t, write.Revision, sizes.Revision)
	require.True(t, tools["memory_revert"].OwnerOnly)
	call("memory_revert", `{"commit":"`+write.Revision+`"}`)
	require.NoError(t, json.Unmarshal(call("memory_sizes", `{}`), &sizes))
	require.Zero(t, sizes.FileCount)
	call("memory_write", `{"path":"topics/delete.md","content":"obsolete","message":"Record"}`)
	call("memory_write", `{"path":"topics/delete.md","delete":true,"message":"Forget"}`)
	for _, input := range []string{
		`{"path":"core.md","message":"Missing action"}`,
		`{"path":"core.md","content":"ambiguous","delete":true,"message":"Two actions"}`,
	} {
		_, err := tools["memory_write"].Handler(owner, json.RawMessage(input))
		require.ErrorContains(t, err, "exclusively")
	}
}
