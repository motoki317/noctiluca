package core

import (
	"fmt"
	"strings"
)

func (c *core) toolNotes() string {
	var notes strings.Builder
	notes.WriteString("One session covers a channel and all its threads. Inputs name the sender, place, and local time, and end the header with refs: `conversation` is where the input was posted, or the ref it was scheduled with; any other ref is named and explained in its messaging entry's notes. " +
		"Process queued conversation messages delivered during tool calls. Each has its own sender and refs. " +
		"Reply only when addressed (a mention, a request to you, or a DM). " +
		"Stay silent for chatter between other people. Copy refs exactly. " +
		"Give watch and wake_at the ref where the result should be posted. A native background completion continues the input that started it. " +
		"A message sent during a tool call reaches you only after the call returns, so run anything that may take over a minute, such as a build, a test suite, or a subagent, with your background tools or watch, and end the turn immediately. Do not wait or poll. " +
		"Reply attachments must come from the same messaging entry as the ref you post to (its first segment).\n\n")
	notes.WriteString(c.replyTool().Description)
	for _, instance := range c.messaging.instances {
		if text := instance.config.Adapter.ToolNotes(); text != "" {
			fmt.Fprintf(&notes, "\n\n%s:\n%s", instance.config.Name, text)
		}
	}
	return notes.String()
}
