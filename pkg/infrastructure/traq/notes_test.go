package traq

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRefNotesAndDescriptions(t *testing.T) {
	for _, name := range []string{"traq", "team"} {
		a := &adapter{name: name}
		require.Equal(t, fmt.Sprintf("traQ has no threads. A ref ending in a colon is a channel. A channel input (a message or a stamp) also has a `quote` ref: posting there posts in the channel and quotes the message. Post chat at `conversation` and work at `quote`. In a DM, post at `conversation`. With %[1]s_send or %[1]s_upload_file, set quote_message_id to the message ID at the end of the `quote` ref you post to. Tool arguments are bare IDs. Use %[1]s_read_message, %[1]s_read_history, and %[1]s_search for context. Text uses Markdown and @username mentions. The 10000-character limit includes file and quote URLs. A file ID is the UUID in a /files/<id> URL in message text.", name), a.ToolNotes())
		for _, tool := range a.Tools() {
			switch tool.Name {
			case name + "_send":
				require.Equal(t, "Post Markdown in a channel.", tool.Description)
			case name + "_upload_file":
				require.Equal(t, "Upload a local file and post its URL.", tool.Description)
			}
		}
	}
}
