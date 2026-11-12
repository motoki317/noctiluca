package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConversationRef(t *testing.T) {
	for _, tc := range []struct{ name, channel, target string }{
		{"chat", "channel", ""},
		{"chat", "channel", "message"},
	} {
		ref := NewConversationRef(tc.name, tc.channel, tc.target)
		t.Run(string(ref), func(t *testing.T) {
			name, channel, target, ok := ParseConversationRef(ref)
			require.True(t, ok)
			require.Equal(t, tc.name, name)
			require.Equal(t, tc.channel, channel)
			require.Equal(t, tc.target, target)
		})
	}
	for _, ref := range []ConversationRef{"chat:channel", "chat:channel:message:extra"} {
		t.Run(string(ref), func(t *testing.T) {
			_, _, _, ok := ParseConversationRef(ref)
			require.False(t, ok)
		})
	}
}
