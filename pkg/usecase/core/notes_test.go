package core

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRefNotesAndReplyDescription(t *testing.T) {
	c := &core{messaging: &messagingRouter{}}
	paragraph := "One session covers a channel and all its threads. Inputs name the sender, place, and local time, and end the header with refs: `conversation` is where the input was posted, or the ref it was scheduled with; any other ref is named and explained in its messaging entry's notes. Process queued conversation messages delivered during tool calls. Each has its own sender and refs. Reply only when addressed (a mention, a request to you, or a DM). Stay silent for chatter between other people. Copy refs exactly. Give watch and wake_at the ref where the result should be posted. A native background completion continues the input that started it. Run long work with your background tools or watch and end the turn immediately. Do not wait or poll. Reply attachments must come from the same messaging entry as the ref you post to (its first segment)."
	description := "Post only when addressed or reporting requested work. Pass a ref from the input, or `owner` for the owner's DM. Each messaging entry's notes say which ref to use.\nIf the message arrived before your last reply, answer only what it adds. Do not repeat your previous reply. If a follow-up asks to append a phrase, send only that phrase once the answer is posted."
	require.Equal(t, paragraph+"\n\n"+description, c.toolNotes())
	tool := c.replyTool()
	require.Equal(t, description, tool.Description)
	var schema struct {
		Properties map[string]struct{ Description string }
	}
	require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
	require.Equal(t, "A ref from the input, or owner.", schema.Properties["target"].Description)
	for _, rule := range []string{"Chat is", "work is", "chat at", "work at"} {
		require.NotContains(t, c.toolNotes(), rule)
	}
}
