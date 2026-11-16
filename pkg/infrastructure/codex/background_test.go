package codex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func commandNote(t *testing.T, s *session, method, turnID, id, status, output string) domain.ModelEvent {
	t.Helper()
	params, err := json.Marshal(map[string]any{"threadId": "thread-1", "turnId": turnID,
		"item": map[string]string{"type": "commandExecution", "id": id, "status": status, "aggregatedOutput": output}})
	require.NoError(t, err)
	control(t, s, "notify", rpcFrame{Method: method, Params: params})
	return nextEvent(t, s)
}

func TestBackgroundCommandCompletion(t *testing.T) {
	s, _ := openInputSession(t, "input-normal")
	sendAndStart(t, s, "start commands")
	control(t, s, "notify", rpcFrame{Method: "item/started", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"type":"mcpToolCall","id":"mcp-1","server":"noctiluca","tool":"watch"}}`)})
	require.Equal(t, domain.ModelEventToolCall, nextEvent(t, s).Kind)
	control(t, s, "notify", rpcFrame{Method: "item/started", Params: json.RawMessage(`{"threadId":"other-thread","turnId":"other-turn","item":{"type":"commandExecution","id":"other-command"}}`)})
	for _, id := range []string{"foreground", "background-1", "background-2", "background-1"} {
		event := commandNote(t, s, "item/started", "turn-1", id, "inProgress", "")
		require.Equal(t, domain.ModelEventToolCall, event.Kind)
		require.Equal(t, id, event.ToolCall.ID)
	}
	foreground := commandNote(t, s, "item/completed", "turn-1", "foreground", "completed", "foreground output")
	require.Equal(t, domain.ModelEventToolResult, foreground.Kind)
	require.Equal(t, "foreground output", foreground.Text)
	control(t, s, "complete", nil)
	require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
	require.False(t, s.Idle(), "commands survive their turn")

	sendAndStart(t, s, "next turn")
	first := commandNote(t, s, "item/completed", "turn-1", "background-1", "completed", "background output")
	require.Equal(t, domain.ModelEventBackgroundDone, first.Kind)
	require.Equal(t, "background-1", first.ToolCall.ID)
	require.Equal(t, "background output", first.Text)
	require.NoError(t, first.Err)
	require.False(t, s.Idle())
	control(t, s, "complete", nil)
	require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
	require.False(t, s.Idle(), "another background command is still running")
	last := commandNote(t, s, "item/completed", "turn-1", "background-2", "failed", "failure output")
	require.Equal(t, domain.ModelEventBackgroundDone, last.Kind)
	require.Equal(t, "failure output", last.Text)
	require.ErrorContains(t, last.Err, "failed")
	require.Eventually(t, s.Idle, time.Second, time.Millisecond)

	repeated := commandNote(t, s, "item/completed", "turn-1", "background-2", "failed", "failure output")
	require.Equal(t, domain.ModelEventToolResult, repeated.Kind)
	require.True(t, s.Idle(), "duplicate starts or completions must not retain a command")
}

func TestSubagentCompletionWakesOnlyIdleParent(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "running"}[running], func(t *testing.T) {
			s, dir := openInputSession(t, "input-normal")
			sendAndStart(t, s, "delegate")
			if !running {
				control(t, s, "complete", nil)
				require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
				require.True(t, s.Idle())
			}
			for _, params := range []string{
				`{"threadId":"other-thread","turnId":"turn-1","item":{"type":"subAgentActivity","kind":"completed"}}`,
				`{"threadId":"thread-1","turnId":"turn-1","item":{"type":"subAgentActivity","kind":"started"}}`,
			} {
				control(t, s, "notify", rpcFrame{Method: "item/completed", Params: json.RawMessage(params)})
			}
			completed := json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"type":"subAgentActivity","kind":"completed","agentThreadId":"child-1"}}`)
			control(t, s, "notify", rpcFrame{Method: "item/started", Params: completed})
			require.Empty(t, s.events, "only a completed activity on the parent thread can wake it")
			control(t, s, "notify", rpcFrame{Method: "item/completed", Params: completed})
			require.Equal(t, domain.ModelEventBackgroundDone, nextEvent(t, s).Kind)
			if !running {
				require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
				require.Equal(t, "turn/start:A subagent finished. Its result is available.", nextEvent(t, s).Text)
			}
			require.False(t, s.Idle())
			control(t, s, "complete", nil)
			require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
			require.Eventually(t, s.Idle, time.Second, time.Millisecond)
			require.Empty(t, s.events, "an internal wake note has no inbox acknowledgement")
			require.NoError(t, s.Close())
			starts := 0
			for _, frame := range requests(t, dir) {
				if frame.Method == "turn/start" {
					starts++
					var params struct{ ThreadID string }
					require.NoError(t, json.Unmarshal(frame.Params, &params))
					require.Equal(t, "thread-1", params.ThreadID)
				}
				require.NotEqual(t, "turn/steer", frame.Method, "a completion during a running turn needs no input")
			}
			require.Equal(t, map[bool]int{false: 2, true: 1}[running], starts)
		})
	}
}
