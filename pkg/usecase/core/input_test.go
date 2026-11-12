package core

import (
	"context"
	"fmt"
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
	info := domain.SourceInfo{Conversation: "#general", Sender: "Alex", ThreadAt: at.Add(-time.Minute), ThreadText: "What did\nyou run?"}
	require.Equal(t, "#general · thread 16:28:01 JST \"What did you run?\" | Alex (owner) | 16:29:01 JST\nmessage: channel=channel id=message\nrefs: conversation=thread\n\nadd 了解 at the end", formatInput(item, domain.InputContext{}, info, nil, "owner", zone).Text)
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
			require.Contains(t, formatInput(item, tt.history, info, nil, "owner", zone).Text, tt.want)
		})
	}
	require.NotContains(t, formatInput(item, domain.InputContext{ThreadSeen: true, Root: &domain.InboundEvent{Deleted: true}}, info, nil, "owner", zone).Text, "root deleted")
	event.Parent, event.Conversation, event.Direct = "", "room", true
	require.Contains(t, formatInput(item, domain.InputContext{}, domain.SourceInfo{Conversation: "Alex", Sender: "Alex"}, nil, "owner", zone).Text, "DM with Alex · top level")
	item.Payload.Event = nil
	item.Payload.MessageRef = "message"
	item.Payload.Context = domain.SessionContext{Conversation: "room"}
	item.Payload.Target, item.ReadyAt, item.SourceKey = "thread", at, "watch:1"
	item.Payload.Input.Text = "exit code 0: DONE"
	for _, source := range []string{"watch:1", "wake:1"} {
		item.SourceKey = source
		input := formatInput(item, domain.InputContext{ThreadSeen: true}, info, nil, "owner", zone)
		require.Contains(t, input.Text, "\nrefs: conversation=thread\n\nexit code 0: DONE")
		require.NotContains(t, input.Text, "\nmessage:")
	}
	item.Lane, item.Payload.Context.TaskName = "task", "heartbeat"
	item.Payload.MessageRef = "ignored"
	require.Contains(t, formatInput(item, domain.InputContext{}, info, nil, "owner", zone).Text, "Task heartbeat | wake |")
	require.Contains(t, formatInput(item, domain.InputContext{}, info, nil, "owner", zone).Text, "\nrefs: conversation=owner\n\n")
	require.Equal(t, strings.Repeat("あ", 80)+"…", excerpt(strings.Repeat("あ", 81)))
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
			input := formatInput(item, domain.InputContext{}, domain.SourceInfo{}, nil, "owner", time.UTC)
			require.Contains(t, input.Text, "\n"+tc.notice+"\nmessage: channel=channel id=message\n")
		})
	}
}

type inputEvents struct{ domain.EventRepository }

func (inputEvents) InputContext(context.Context, domain.SessionKey, domain.ConversationRef, time.Time) (domain.InputContext, error) {
	return domain.InputContext{}, nil
}

type postRefsMessaging struct {
	*fakeMessaging
	refs func(domain.ConversationRef, domain.ConversationRef, bool) []domain.NamedRef
}

func (m postRefsMessaging) PostRefs(conversation, message domain.ConversationRef, direct bool) []domain.NamedRef {
	return m.refs(conversation, message, direct)
}

func TestPreparePostRefs(t *testing.T) {
	for _, tc := range []struct {
		name, lane, target string
		event, direct      bool
		wantCall           bool
	}{
		{"event", "conv", "entry:room:", true, false, true},
		{"direct event", "conv", "entry:dm:", true, true, true},
		{"continuation", "conv", "entry:room:thread", false, false, true},
		{"direct continuation", "conv", "entry:dm:", false, true, true},
		{"owner", "conv", "owner", false, false, false},
		{"task", "task", "entry:room:", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			adapter := postRefsMessaging{fakeMessaging: &fakeMessaging{}, refs: func(conversation, message domain.ConversationRef, direct bool) []domain.NamedRef {
				called = true
				require.Equal(t, domain.ConversationRef(tc.target), conversation)
				require.Equal(t, domain.ConversationRef("message"), message)
				require.Equal(t, tc.direct, direct)
				return []domain.NamedRef{{Name: "extra", Ref: "extra-ref"}, {Name: "another", Ref: "second-ref"}}
			}}
			router, err := newMessagingRouter([]Messaging{{Name: "entry", Owner: "owner", Adapter: adapter}})
			require.NoError(t, err)
			c := &core{events: inputEvents{}, messaging: router}
			payload := domain.InboxPayload{Target: domain.ConversationRef(tc.target), MessageRef: "message", Context: domain.SessionContext{Conversation: "entry:room:", Direct: tc.direct}}
			if tc.event {
				event := message(domain.ConversationRef(tc.target), "owner", "input")
				event.MessageRef, event.Direct = "message", tc.direct
				payload.Event = &event
				payload.Target, payload.MessageRef, payload.Context.Direct = "ignored", "ignored", !tc.direct
			}
			input, err := c.prepare(t.Context(), domain.InboxItem{Lane: tc.lane, Payload: payload})
			require.NoError(t, err)
			require.Equal(t, tc.wantCall, called)
			if tc.wantCall {
				require.Contains(t, input.Text, "\nrefs: conversation="+tc.target+" extra=extra-ref another=second-ref\n\n")
				if !tc.event {
					require.NotContains(t, input.Text, "\nmessage:")
				}
			} else {
				require.NotContains(t, input.Text, " extra=")
			}
			require.NotContains(t, input.Text, " message=")
		})
	}
}

func TestInputReactionAndMissingMessageRef(t *testing.T) {
	event := message("room", "member", "reacted")
	event.Kind, event.MessageRef = domain.InboundEventReaction, "entry:channel:message"
	item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event, Target: "ignored", MessageRef: "ignored"}}
	input := formatInput(item, domain.InputContext{LastReply: event.ReceivedAt.Add(time.Second)}, domain.SourceInfo{}, nil, "owner", time.UTC)
	require.Contains(t, input.Text, "in this conversation.\nrefs: conversation=room\n\n")
	require.Contains(t, input.Text, "\nReaction.\n")
	require.NotContains(t, input.Text, "\nmessage:")
	for _, kind := range []domain.InboundEventKind{domain.InboundEventMessage, domain.InboundEventMessageEdited} {
		for _, ref := range []domain.ConversationRef{"", "message", "entry:channel", "entry:channel:message:extra"} {
			t.Run(fmt.Sprintf("%s/%s", kind, ref), func(t *testing.T) {
				event.Kind, event.MessageRef = kind, ref
				input = formatInput(item, domain.InputContext{}, domain.SourceInfo{}, nil, "owner", time.UTC)
				require.Contains(t, input.Text, "\nrefs: conversation=room\n\n")
				require.NotContains(t, input.Text, "\nmessage:")
			})
		}
	}
}

func TestReadableInputKeepsKnownSenderName(t *testing.T) {
	event := message("room", "B1", "hello")
	event.Sender.DisplayName = "Helpful bot"
	item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event}}
	input := formatInput(item, domain.InputContext{}, domain.SourceInfo{Sender: "B1"}, nil, "owner", time.UTC)
	require.Contains(t, input.Text, "| Helpful bot |")
	require.NotContains(t, input.Text, "B1")
}
