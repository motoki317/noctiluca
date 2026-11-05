package core

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	memoryusecase "github.com/motoki317/noctiluca/pkg/usecase/memory"
	"github.com/stretchr/testify/require"
)

func TestMemoryPromptRefreshAndTools(t *testing.T) {
	s, err := memory.New(t.Context(), memory.Config{Path: filepath.Join(t.TempDir(), "memory")})
	require.NoError(t, err)
	owner := domain.WithToolSession(t.Context(), domain.ToolSession{Key: "dm", Actor: domain.ActorOwner})
	require.NoError(t, s.Write(owner, "owner/persona.md", "Persona rules", "Set persona"))
	require.NoError(t, s.Write(owner, "core.md", "Initial index", "Set core"))
	require.NoError(t, s.Write(owner, "topics/secret.md", "On demand only", "Record topic"))
	require.NoError(t, s.Write(owner, "procedures/heartbeat.md", "Heartbeat task", "Set heartbeat"))
	f := setupMemory(t, Config{Owner: "owner", Rules: map[domain.ConversationRef]domain.AcceptRule{"dm": {Users: []string{"owner"}}}}, nil, nil, nil, s)
	f.messaging.in <- message("dm", "owner", "first")
	model := take(t, f.model.opened)
	input := take(t, model.inputs)
	require.Contains(t, input.Text, "Persona rules")
	require.Contains(t, input.Text, "Initial index")
	require.NotContains(t, input.Text, "On demand only")
	require.NotContains(t, input.Text, "Heartbeat task")
	write := f.hub.tools["memory_write"]
	_, err = write.Handler(owner, json.RawMessage(`{"path":"core.md","content":"Updated index","message":"Update core"}`))
	require.NoError(t, err)
	model.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.messaging.in <- message("dm", "owner", "second")
	input = take(t, model.inputs)
	require.Contains(t, input.Text, "Updated index")
	require.NotContains(t, input.Text, "Initial index")
	result, err := f.hub.tools["memory_read"].Handler(owner, json.RawMessage(`{"path":"procedures/heartbeat.md"}`))
	require.NoError(t, err)
	require.Contains(t, string(result), "Heartbeat task")
	result, err = f.hub.tools["memory_search"].Handler(owner, json.RawMessage(`{"query":"On demand"}`))
	require.NoError(t, err)
	require.Contains(t, string(result), "topics/secret.md")
	member := domain.WithToolSession(t.Context(), domain.ToolSession{Key: "room", Actor: domain.ActorMember})
	_, err = write.Handler(member, json.RawMessage(`{"path":"owner/persona.md","content":"forged","message":"Bypass owner"}`))
	require.ErrorContains(t, err, "owner permission")
	args, err := json.Marshal(map[string]string{"path": "core.md", "content": strings.Repeat("x", 8193), "message": "Too large"})
	require.NoError(t, err)
	_, err = write.Handler(owner, args)
	require.ErrorContains(t, err, "merge or delete")
}

func TestMemoryPromptPreservesInputAndNotes(t *testing.T) {
	seen := false
	prompt := memoryusecase.Prompt(nil, "Use adapter tools", func(_ context.Context, _ domain.ToolSession, input domain.UserInput) (domain.UserInput, error) {
		seen = true
		input.Text = "hook: " + input.Text
		return input, nil
	})
	input := domain.UserInput{Text: "message", Attachments: []domain.Attachment{{Ref: "file"}}}
	out, err := prompt(t.Context(), domain.ToolSession{Actor: domain.ActorMember}, input)
	require.NoError(t, err)
	require.True(t, seen)
	require.Contains(t, out.Text, "Use adapter tools")
	require.Contains(t, out.Text, "hook: message")
	require.Equal(t, input.Attachments, out.Attachments)
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
