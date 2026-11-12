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
		{`{"type":"stream_event","parent_tool_use_id":"tool-1","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"child"}}}`, nil},
		{`{"type":"assistant","parent_tool_use_id":"tool-1","message":{"content":[{"type":"tool_use","name":"child"}]}}`, nil},
		{`{"type":"result","subtype":"success"}`, []domain.ModelEventKind{domain.ModelEventTurnFinished}},
		{`{"type":"system","subtype":"task_notification","task_id":"task-1","status":"completed","summary":"done"}`, []domain.ModelEventKind{domain.ModelEventBackgroundDone}},
		{`{"type":"stream_event","event":{"type":"message_start"}}`, []domain.ModelEventKind{domain.ModelEventTurnStarted}},
	}
	for _, tc := range cases {
		f, err := parseFrame([]byte(tc.line))
		require.NoError(t, err)
		events := p.events(f, nil)
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
		{`{"type":"system","subtype":"task_started","task_id":"foreground","is_backgrounded":false}`, 1},
		{`{"type":"system","subtype":"task_started","task_id":"b","is_backgrounded":true}`, 2},
		{`{"type":"system","subtype":"task_notification","task_id":"a","status":"completed"}`, 1},
		{`{"type":"result","subtype":"success"}`, 1},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`, 0},
	} {
		f, err := parseFrame([]byte(tc.line))
		require.NoError(t, err)
		p.events(f, nil)
		require.Len(t, p.tasks, tc.pending)
	}
}

func TestRecoverableToolError(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{"string", `" \nExit code 1\nfile not found\n\t "`, "Exit code 1\nfile not found"},
		{"mixed blocks", `[{"type":"image","text":"ignored"},{"type":"text","text":"read failed"},{"type":"text","text":""},{"type":"text","text":"permission denied"}]`, "read failed\n\npermission denied"},
		{"blank string", `" \t\r\n "`, "model tool failed"},
		{"unsupported content", `{"text":"ignored"}`, "model tool failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parseFrame([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call","is_error":true,"content":` + tc.content + `}]}}`))
			require.NoError(t, err)
			var p parser
			events := p.events(f, nil)
			require.Len(t, events, 1)
			require.Equal(t, domain.ModelEventToolResult, events[0].Kind)
			require.Equal(t, "call", events[0].ToolCall.ID)
			require.EqualError(t, events[0].Err, tc.want)
		})
	}
}

func TestFailedResultDoesNotExposeBody(t *testing.T) {
	for _, f := range []frame{{Type: "result", IsError: true, Result: "private body"}, {Type: "result", Subtype: "error_during_execution", Result: "private body"}} {
		var p parser
		events := p.events(f, nil)
		require.Len(t, events, 2)
		require.Equal(t, domain.ModelEventTurnFinished, events[1].Kind)
		require.Empty(t, events[1].Text)
		require.Contains(t, events[0].Err.Error(), authRemedy)
	}
	var p parser
	require.Equal(t, "answer", p.events(frame{Type: "result", Result: "answer"}, nil)[0].Text)
}

func TestAutonomousTurnWithoutInput(t *testing.T) {
	var p parser
	p.events(frame{Type: "result", Subtype: "success"}, nil)
	events := p.events(frame{Type: "system", Subtype: "task_notification", TaskID: "background", Status: "completed"}, nil)
	require.Equal(t, domain.ModelEventBackgroundDone, events[0].Kind)
	f, err := parseFrame([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"reply","name":"reply","input":{"target":"thread","text":"done"}}]}}`))
	require.NoError(t, err)
	events = p.events(f, nil)
	require.Len(t, events, 2)
	require.Equal(t, domain.ModelEventTurnStarted, events[0].Kind)
	require.Equal(t, domain.ModelEventToolCall, events[1].Kind)
	require.True(t, p.active)
	require.Equal(t, domain.ModelEventTurnFinished, p.events(frame{Type: "result"}, nil)[0].Kind)
	require.False(t, p.active)
}

func TestInputLifecycle(t *testing.T) {
	var p parser
	pending := map[string]int64{"input": 1, "refused": 2, "discarded": 3, "cancelled": 4}
	for _, tc := range []struct {
		state, uuid string
		kinds       []domain.ModelEventKind
	}{
		{"queued", "input", nil},
		{"started", "input", []domain.ModelEventKind{domain.ModelEventInputStarted, domain.ModelEventTurnStarted}},
		{"started", "input", nil},
		{"completed", "input", nil},
		{"started", "unknown", []domain.ModelEventKind{domain.ModelEventTurnStarted}},
		{"refused", "refused", []domain.ModelEventKind{domain.ModelEventInputRefused}},
		{"discarded", "discarded", []domain.ModelEventKind{domain.ModelEventInputRefused}},
		{"cancelled", "cancelled", []domain.ModelEventKind{domain.ModelEventInputRefused}},
	} {
		id := pending[tc.uuid]
		var kinds []domain.ModelEventKind
		for _, event := range p.events(frame{Type: "command_lifecycle", State: tc.state, CommandUUID: tc.uuid}, pending) {
			kinds = append(kinds, event.Kind)
			if event.Kind == domain.ModelEventInputStarted || event.Kind == domain.ModelEventInputRefused {
				require.Equal(t, id, event.InputID)
				require.NotContains(t, pending, tc.uuid)
				if event.Kind == domain.ModelEventInputRefused {
					require.Error(t, event.Err)
				}
			}
		}
		require.Equal(t, tc.kinds, kinds, tc.state+"/"+tc.uuid)
	}
}
