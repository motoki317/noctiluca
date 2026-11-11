package discord

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRefNotes(t *testing.T) {
	for _, name := range []string{"discord", "community"} {
		a := &adapter{name: name}
		require.Equal(t, fmt.Sprintf("Channel, thread, and message arguments are bare numeric IDs. A ref ending in a colon is a channel. In a ref with a final ID, that ID is a thread or a message. A top-level channel input (a message or a reaction) also has a `thread` ref: posting there starts a thread from the message. Talk at `conversation`, including quick investigations and their results. Use `thread` only for a project that spans days, or when asked for a thread. Other inputs have only `conversation`; post there. Use %[1]s_read_history and %[1]s_read_thread for context. Only user mentions notify. If an answer needs any tool besides the reply, first show that you started: react to the input `message` with %[1]s_react, whose middle ID is the channel and last ID the message.", name), a.ToolNotes())
	}
}
