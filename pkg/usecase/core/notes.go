package core

import (
	"fmt"
	"strings"
)

func (c *core) toolNotes() string {
	var notes strings.Builder
	notes.WriteString("One session covers a channel and all its threads. Inputs name the sender, place, local time, and reply/work targets. " +
		"Process queued conversation messages delivered during tool calls. Each has its own sender and targets. " +
		"Reply only when addressed (a mention, a request to you, or a DM). " +
		"Stay silent for chatter between other people. Copy targets exactly. " +
		"Use the work target for watch and wake_at. Native background completions use the work target saved from their request. " +
		"Run long work with your background tools or watch and end the turn immediately. Do not wait or poll. " +
		"Reply attachments must come from the same messaging entry as the reply target (the target's first segment).\n\n")
	notes.WriteString(c.replyTool().Description)
	for _, instance := range c.messaging.instances {
		if text := instance.config.Adapter.ToolNotes(); text != "" {
			fmt.Fprintf(&notes, "\n\n%s:\n%s", instance.config.Name, text)
		}
	}
	return notes.String()
}
