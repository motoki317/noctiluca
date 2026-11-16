package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/stretchr/testify/require"
)

func TestReadableInput(t *testing.T) {
	at := time.Date(2026, 11, 6, 7, 29, 1, 0, time.UTC)
	zone := time.FixedZone("JST", 9*60*60)
	event := domain.InboundEvent{Kind: domain.InboundEventMessage, Conversation: "thread", Parent: "room", MessageRef: "entry:channel:message", Sender: domain.Sender{ID: "owner"}, Text: "add 了解 at the end", ReceivedAt: at}
	item := domain.InboxItem{Lane: "conv", Payload: domain.InboxPayload{Event: &event}}
	info := domain.SourceInfo{Location: "entry: native IDs", Conversation: "#general", Sender: "Alex", ThreadAt: at.Add(-time.Minute), ThreadText: "What did\nyou run?"}
	require.Equal(t, "#general · thread 16:28:01 JST \"What did you run?\" | Alex (owner) | 16:29:01 JST\nentry: native IDs\n\nadd 了解 at the end", formatInput(item, domain.InputContext{}, info, "owner", zone).Text)
	for _, tt := range []struct {
		name    string
		history domain.InputContext
		want    string
	}{
		{"deleted root", domain.InputContext{Root: &domain.InboundEvent{Deleted: true}}, "(root deleted)"},
		{"before reply", domain.InputContext{ThreadSeen: true, Running: true, LastReply: at.Add(time.Second)}, "Sent at 16:29:01 JST, before your reply at 16:29:02 JST in this thread."},
		{"queued", domain.InputContext{ThreadSeen: true, Running: true}, "Sent while your previous turn was running."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Contains(t, formatInput(item, tt.history, info, "owner", zone).Text, tt.want)
		})
	}
	require.NotContains(t, formatInput(item, domain.InputContext{ThreadSeen: true, Root: &domain.InboundEvent{Deleted: true}}, info, "owner", zone).Text, "root deleted")
	event.Parent, event.Conversation, event.Direct = "", "room", true
	require.Contains(t, formatInput(item, domain.InputContext{}, domain.SourceInfo{Conversation: "Alex", Sender: "Alex"}, "owner", zone).Text, "DM with Alex · top level")
	item.Payload.Event = nil
	item.Lane, item.Payload.Context.TaskName, item.ReadyAt = "task", "heartbeat", at
	require.Equal(t, "Task heartbeat | system | 16:29:01 JST\n\n", formatInput(item, domain.InputContext{}, info, "owner", zone).Text)
	require.Equal(t, strings.Repeat("あ", 80)+"…", excerpt(strings.Repeat("あ", 81)))
}

func TestPrepareBackgroundInput(t *testing.T) {
	for _, source := range []string{"watch:1", "wake:1"} {
		item := domain.InboxItem{SourceKey: source, Payload: domain.InboxPayload{Input: domain.UserInput{Text: "completed work"}}}
		input, err := (&core{}).prepare(t.Context(), item)
		require.NoError(t, err)
		require.Equal(t, item.Payload.Input, input)
	}
}

func TestReadableMessageChanges(t *testing.T) {
	for _, tc := range []struct {
		name, notice string
		deleted      bool
	}{
		{"empty edit", "Message edited.", false},
		{"deleted", "Message deleted.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := domain.InboundEvent{
				Kind: domain.InboundEventMessageEdited, Deleted: tc.deleted,
				Conversation: "room", MessageRef: "entry:channel:message",
			}
			item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event}}
			input := formatInput(item, domain.InputContext{}, domain.SourceInfo{}, "owner", time.UTC)
			require.Contains(t, input.Text, "\n"+tc.notice+"\n")
		})
	}
}

type inputEvents struct{ domain.EventRepository }

func (inputEvents) InputContext(context.Context, domain.SessionKey, domain.ConversationRef, time.Time) (domain.InputContext, error) {
	return domain.InputContext{}, nil
}

func TestPrepareMessagingEventWithBackgroundPrefix(t *testing.T) {
	adapter := newNamedMessaging("watch")
	router, err := newMessagingRouter([]Messaging{{Name: "watch", Adapter: adapter, Rules: map[domain.ConversationRef]domain.AcceptRule{"watch:room": {Users: []string{"owner"}}}}})
	require.NoError(t, err)
	c := &core{events: inputEvents{}, messaging: router}
	event := message("watch:room", "owner", "hello")
	input, err := c.prepare(t.Context(), domain.InboxItem{SourceKey: "watch:message", Payload: domain.InboxPayload{Event: &event}})
	require.NoError(t, err)
	require.Contains(t, input.Text, "hello")
	require.Contains(t, input.Text, "watch user")
}

func TestInputReaction(t *testing.T) {
	event := message("room", "member", "reacted")
	event.Kind, event.MessageRef = domain.InboundEventReaction, "entry:channel:message"
	item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event}}
	input := formatInput(item, domain.InputContext{LastReply: event.ReceivedAt.Add(time.Second)}, domain.SourceInfo{}, "owner", time.UTC)
	require.Contains(t, input.Text, "in this conversation.\n\n")
	require.Contains(t, input.Text, "\nReaction.\n")
	require.NotContains(t, input.Text, "\nmessage:")
}

func TestReadableInputKeepsKnownSenderName(t *testing.T) {
	event := message("room", "B1", "hello")
	event.Sender.DisplayName = "Helpful bot"
	item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event}}
	input := formatInput(item, domain.InputContext{}, domain.SourceInfo{Sender: "B1"}, "owner", time.UTC)
	require.Contains(t, input.Text, "| Helpful bot |")
	require.NotContains(t, input.Text, "B1")
}
