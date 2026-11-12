package codex

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func TestProtocolToolEvents(t *testing.T) {
	data, err := json.Marshal(map[string]any{"type": "commandExecution", "id": "tool-1", "status": "failed", "command": "false"})
	require.NoError(t, err)
	events, err := notificationEvents("item/started", notification{Item: data})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, domain.ModelEventToolCall, events[0].Kind)
	require.Equal(t, "commandExecution", events[0].ToolCall.Name)
	require.JSONEq(t, string(data), string(events[0].ToolCall.Input))
	events, err = notificationEvents("item/completed", notification{Item: data})
	require.NoError(t, err)
	require.Equal(t, domain.ModelEventToolResult, events[0].Kind)
	require.Equal(t, "tool-1", events[0].ToolCall.ID)
	require.ErrorContains(t, events[0].Err, "failed")
	_, err = notificationEvents("item/started", notification{Item: json.RawMessage(`"invalid"`)})
	require.Error(t, err)
	for _, method := range []string{"item/started", "item/completed"} {
		events, err := notificationEvents(method, notification{Item: json.RawMessage(`{"type":"agentMessage","text":"already streamed"}`)})
		require.NoError(t, err)
		require.Empty(t, events)
	}
}

func TestProtocolErrorMessages(t *testing.T) {
	for _, retry := range []bool{true, false} {
		var n notification
		data, err := json.Marshal(map[string]any{"error": map[string]string{"message": "limit reached", "codexErrorInfo": "usageLimitExceeded"}, "willRetry": retry})
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &n))
		events, err := notificationEvents("error", n)
		require.NoError(t, err)
		if retry {
			require.Empty(t, events)
		} else {
			require.Len(t, events, 1)
			require.Equal(t, domain.ModelEventError, events[0].Kind)
			require.EqualError(t, events[0].Err, "limit reached")
		}
	}
	var n notification
	require.NoError(t, json.Unmarshal([]byte(`{"turn":{"id":"turn-1","status":"failed","error":{"message":"plan unavailable"}}}`), &n))
	events, err := notificationEvents("turn/completed", n)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.EqualError(t, events[0].Err, "plan unavailable")
	require.Equal(t, domain.ModelEventTurnFinished, events[1].Kind)
}
