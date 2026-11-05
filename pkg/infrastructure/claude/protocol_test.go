// Claude CLI protocol reference: OpenClaw (MIT), https://github.com/openclaw/openclaw.
// Go reimplementation. Provenance and license notice: THIRD_PARTY_NOTICES.md.

package claude

import (
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/stretchr/testify/require"
)

func TestProtocol(t *testing.T) {
	var p parser
	cases := []struct {
		line  string
		kinds []domain.ModelEventKind
	}{
		{`{"type":"user","isReplay":true,"message":{"role":"user","content":"hello"}}`, nil},
		{`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`, nil},
		{`{"type":"command_lifecycle","state":"started"}`, []domain.ModelEventKind{domain.ModelEventTurnStarted}},
		{`{"type":"stream_event","event":{"type":"message_start"}}`, nil},
		{`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`, []domain.ModelEventKind{domain.ModelEventText}},
		{`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`, nil},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-1","name":"reply","input":{"text":"hello"}}]}}`, []domain.ModelEventKind{domain.ModelEventToolCall}},
		{`{"type":"stream_event","event":{"type":"message_stop"}}`, nil},
		{`{"type":"stream_event","event":{"type":"message_start"}}`, nil},
		{`{"type":"stream_event","parent_tool_use_id":"tool-1","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"child"}}}`, nil},
		{`{"type":"assistant","parent_tool_use_id":"tool-1","message":{"content":[{"type":"tool_use","name":"child"}]}}`, nil},
		{`{"type":"result","subtype":"success"}`, []domain.ModelEventKind{domain.ModelEventTurnFinished}},
		{`{"type":"system","subtype":"task_notification","task_id":"task-1","status":"completed","summary":"done"}`, []domain.ModelEventKind{domain.ModelEventBackgroundDone}},
		{`{"type":"stream_event","event":{"type":"message_start"}}`, []domain.ModelEventKind{domain.ModelEventTurnStarted}},
		{`{"type":"result","subtype":"error_during_execution","is_error":true}`, []domain.ModelEventKind{domain.ModelEventError, domain.ModelEventTurnFinished}},
		{`{"type":"keep_alive"}`, nil},
		{`{"type":"command_lifecycle","state":"refused"}`, []domain.ModelEventKind{domain.ModelEventError}},
	}
	for _, tc := range cases {
		f, err := parseFrame([]byte(tc.line))
		require.NoError(t, err)
		events := p.events(f)
		var kinds []domain.ModelEventKind
		for _, event := range events {
			kinds = append(kinds, event.Kind)
			switch event.Kind {
			case domain.ModelEventText:
				require.Equal(t, "hello", event.Text)
			case domain.ModelEventToolCall:
				require.Equal(t, "reply", event.ToolCall.Name)
				require.JSONEq(t, `{"text":"hello"}`, string(event.ToolCall.Input))
			case domain.ModelEventBackgroundDone:
				require.Equal(t, "task-1 (completed): done", event.Text)
			case domain.ModelEventError:
				require.Error(t, event.Err)
			}
		}
		require.Equal(t, tc.kinds, kinds, tc.line)
	}
}

func TestBackgroundActivity(t *testing.T) {
	var p parser
	for _, tc := range []struct {
		line    string
		pending int
	}{
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a"}]}`, 1},
		{`{"type":"system","subtype":"task_started","task_id":"a","is_backgrounded":true}`, 1},
		{`{"type":"system","subtype":"task_started","task_id":"foreground","is_backgrounded":false}`, 1},
		{`{"type":"system","subtype":"task_started","task_id":"b","is_backgrounded":true}`, 2},
		{`{"type":"system","subtype":"task_notification","task_id":"a","status":"completed"}`, 1},
		{`{"type":"system","subtype":"task_notification","task_id":"a","status":"completed"}`, 1},
		{`{"type":"result","subtype":"success"}`, 1},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`, 0},
	} {
		f, err := parseFrame([]byte(tc.line))
		require.NoError(t, err)
		p.events(f)
		require.Len(t, p.tasks, tc.pending)
	}
}

func TestRecoverableToolError(t *testing.T) {
	f, err := parseFrame([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call","is_error":true,"content":"private error body"}]}}`))
	require.NoError(t, err)
	var p parser
	events := p.events(f)
	require.Len(t, events, 1)
	require.Equal(t, domain.ModelEventToolResult, events[0].Kind)
	require.Equal(t, "call", events[0].ToolCall.ID)
	require.EqualError(t, events[0].Err, "model tool failed")
	require.Empty(t, events[0].Text)
}

func TestFailedResultDoesNotExposeBody(t *testing.T) {
	for _, f := range []frame{{Type: "result", IsError: true, Result: "private body"}, {Type: "result", Subtype: "error_during_execution", Result: "private body"}} {
		var p parser
		events := p.events(f)
		require.Len(t, events, 2)
		require.Equal(t, domain.ModelEventTurnFinished, events[1].Kind)
		require.Empty(t, events[1].Text)
		require.Contains(t, events[0].Err.Error(), authRemedy)
	}
	var p parser
	require.Equal(t, "answer", p.events(frame{Type: "result", Result: "answer"})[0].Text)
}

func TestAutonomousTurnWithoutInput(t *testing.T) {
	for _, partial := range []bool{false, true} {
		var p parser
		p.events(frame{Type: "result", Subtype: "success"})
		events := p.events(frame{Type: "system", Subtype: "task_notification", TaskID: "background", Status: "completed"})
		require.Equal(t, domain.ModelEventBackgroundDone, events[0].Kind)
		if partial {
			f, err := parseFrame([]byte(`{"type":"stream_event","event":{"type":"message_start"}}`))
			require.NoError(t, err)
			events = p.events(f)
			require.Equal(t, domain.ModelEventTurnStarted, events[0].Kind)
		}
		f, err := parseFrame([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"reply","name":"reply","input":{"target":"thread","text":"done"}}]}}`))
		require.NoError(t, err)
		events = p.events(f)
		if !partial {
			require.Equal(t, domain.ModelEventTurnStarted, events[0].Kind)
			events = events[1:]
		}
		require.Len(t, events, 1)
		require.Equal(t, domain.ModelEventToolCall, events[0].Kind)
		require.True(t, p.active)
		require.Equal(t, domain.ModelEventTurnFinished, p.events(frame{Type: "result"})[0].Kind)
		require.False(t, p.active)
	}
}
