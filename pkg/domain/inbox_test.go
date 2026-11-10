package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInboxMessageRefCompatibility(t *testing.T) {
	var payload InboxPayload
	require.NoError(t, json.Unmarshal([]byte(`{"Target":"room","Input":{"Text":"note"}}`), &payload))
	require.Equal(t, ConversationRef("room"), payload.Target)
	require.Empty(t, payload.MessageRef)
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "MessageRef")
	payload.MessageRef = "message"
	raw, err = json.Marshal(payload)
	require.NoError(t, err)
	var restored InboxPayload
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.Equal(t, payload, restored)
}
