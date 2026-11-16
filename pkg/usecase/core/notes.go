package core

import (
	"context"
	"fmt"
	"strings"
)

func (c *core) toolNotes(ctx context.Context) string {
	var notes strings.Builder
	notes.WriteString("One session covers a channel and all its threads. Inputs name the sender, place, and local time. " +
		"Each messaging entry supplies the IDs its tools accept in the input header. Use its send tool to post messages. " +
		"Process queued conversation messages delivered during tool calls. Each has its own sender and location. " +
		"Stay silent for chatter between other people. " +
		"Watch and wake_at return their command result or note as a new input. A native background completion continues the input that started it. " +
		"A message sent during a tool call reaches you only after the call returns. " +
		"Use background tools or watch for work that may take over a minute, including builds, test suites, and subagents. " +
		"End the turn immediately. Do not wait or poll.")
	for _, instance := range c.messaging.instances {
		if text := instance.config.Adapter.ToolNotes(ctx); text != "" {
			fmt.Fprintf(&notes, "\n\n%s:\n%s", instance.config.Name, text)
		}
	}
	return notes.String()
}
