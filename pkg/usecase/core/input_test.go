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
	event := domain.InboundEvent{Conversation: "thread", Parent: "room", MessageRef: "message", Sender: domain.Sender{ID: "owner"}, Text: "add 了解 at the end", ReceivedAt: at}
	item := domain.InboxItem{Lane: "conv", Payload: domain.InboxPayload{Event: &event}}
	info := domain.SourceInfo{Conversation: "#general", Sender: "Alex", ThreadAt: at.Add(-time.Minute), ThreadText: "What did\nyou run?"}
	for _, tt := range []struct {
		name    string
		history domain.InputContext
		want    string
	}{
		{"first thread", domain.InputContext{}, "#general · thread 16:28:01 JST \"What did you run?\" | Alex (owner) | 16:29:01 JST\nrefs: conversation=thread\n\nadd 了解 at the end"},
		{"deleted root", domain.InputContext{Root: &domain.InboundEvent{Deleted: true}}, "#general · thread 16:28:01 JST (root deleted) | Alex (owner) | 16:29:01 JST\nrefs: conversation=thread\n\nadd 了解 at the end"},
		{"seen deleted root", domain.InputContext{ThreadSeen: true, Root: &domain.InboundEvent{Deleted: true}}, "#general · thread 16:28:01 JST | Alex (owner) | 16:29:01 JST\nrefs: conversation=thread\n\nadd 了解 at the end"},
		{"before reply", domain.InputContext{ThreadSeen: true, Running: true, LastReply: at.Add(time.Second)}, "#general · thread 16:28:01 JST | Alex (owner) | 16:29:01 JST\nSent at 16:29:01 JST, before your reply at 16:29:02 JST in this thread.\nrefs: conversation=thread\n\nadd 了解 at the end"},
		{"queued", domain.InputContext{ThreadSeen: true, Running: true}, "#general · thread 16:28:01 JST | Alex (owner) | 16:29:01 JST\nSent while your previous turn was running. After its reply, answer only what this message adds.\nrefs: conversation=thread\n\nadd 了解 at the end"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, formatInput(item, tt.history, info, nil, "owner", zone).Text)
		})
	}
	event.Parent, event.Conversation, event.Direct = "", "room", true
	require.Equal(t, "DM with Alex · top level | Alex (owner) | 16:29:01 JST\nrefs: conversation=room\n\nadd 了解 at the end", formatInput(item, domain.InputContext{}, domain.SourceInfo{Conversation: "Alex", Sender: "Alex"}, nil, "owner", zone).Text)
	item.Payload.Event = nil
	item.Payload.Context = domain.SessionContext{Conversation: "room"}
	item.Payload.Target, item.ReadyAt, item.SourceKey = "thread", at, "watch:1"
	item.Payload.Input.Text = "exit code 0: DONE"
	require.Equal(t, "#general · thread 16:28:01 JST | watch | 16:29:01 JST\nrefs: conversation=thread\n\nexit code 0: DONE", formatInput(item, domain.InputContext{ThreadSeen: true}, info, nil, "owner", zone).Text)
	item.SourceKey = "wake:1"
	require.Equal(t, "#general · thread 16:28:01 JST | wake | 16:29:01 JST\nrefs: conversation=thread\n\nexit code 0: DONE", formatInput(item, domain.InputContext{ThreadSeen: true}, info, nil, "owner", zone).Text)
	item.Lane, item.Payload.Context.TaskName = "task", "heartbeat"
	item.Payload.MessageRef = "ignored"
	require.Equal(t, "Task heartbeat | wake | 16:29:01 JST\nrefs: conversation=owner\n\nexit code 0: DONE", formatInput(item, domain.InputContext{}, info, nil, "owner", zone).Text)
	require.Equal(t, strings.Repeat("あ", 80)+"…", excerpt(strings.Repeat("あ", 81)))
}

func TestReadableMessageChanges(t *testing.T) {
	for _, tc := range []struct {
		name, text, notice string
		deleted            bool
	}{
		{"edited", "updated text", "Message edited.", false},
		{"empty edit", "", "Message edited.", false},
		{"deleted", "", "Message deleted.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := domain.InboundEvent{
				Kind: domain.InboundEventMessageEdited, Deleted: tc.deleted,
				Conversation: "room", MessageRef: "message", Sender: domain.Sender{ID: "member"},
				Text: tc.text, ReceivedAt: time.Date(2026, 11, 6, 7, 29, 1, 0, time.UTC),
			}
			item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event}}
			input := formatInput(item, domain.InputContext{}, domain.SourceInfo{}, nil, "owner", time.UTC)
			require.Equal(t, "room · top level | member | 07:29:01 UTC\n"+tc.notice+"\nrefs: conversation=room\n\n"+tc.text, input.Text)
		})
	}
}

func TestInputNamedRefs(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, threaded := range []bool{false, true} {
			t.Run(fmt.Sprintf("direct=%t/threaded=%t", direct, threaded), func(t *testing.T) {
				event := message("room", "member", "start work")
				event.Direct, event.MessageRef = direct, "message-thread"
				if threaded {
					event.Parent, event.Conversation = "room", "existing-thread"
				}
				item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event, Context: domain.SessionContext{Conversation: "room", Direct: direct}}}
				refs := []domain.NamedRef{{Name: "extra", Ref: "first-ref"}, {Name: "another", Ref: "second-ref"}}
				input := formatInput(item, domain.InputContext{}, domain.SourceInfo{}, refs, "owner", time.UTC)
				require.Contains(t, input.Text, fmt.Sprintf("\nrefs: conversation=%s extra=first-ref another=second-ref\n\n", event.Conversation))
				item.Payload.Target, item.Payload.MessageRef, item.Payload.Event = event.Conversation, event.MessageRef, nil
				input = formatInput(item, domain.InputContext{}, domain.SourceInfo{}, refs, "owner", time.UTC)
				require.Contains(t, input.Text, fmt.Sprintf("\nrefs: conversation=%s extra=first-ref another=second-ref\n\n", event.Conversation))
				for _, key := range []string{"watch:1", "wake:1"} {
					item.SourceKey, item.Payload.MessageRef = key, ""
					input = formatInput(item, domain.InputContext{}, domain.SourceInfo{}, nil, "owner", time.UTC)
					require.Contains(t, input.Text, fmt.Sprintf("\nrefs: conversation=%s\n\n", event.Conversation))
				}
			})
		}
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
				return []domain.NamedRef{{Name: "extra", Ref: "extra-ref"}}
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
				require.Contains(t, input.Text, "\nrefs: conversation="+tc.target+" extra=extra-ref\n\n")
			} else {
				require.NotContains(t, input.Text, " extra=")
			}
			require.NotContains(t, input.Text, " message=")
		})
	}
}

func TestPrepareWithoutPostRefProvider(t *testing.T) {
	router, err := newMessagingRouter([]Messaging{{Name: "entry", Adapter: &fakeMessaging{}}})
	require.NoError(t, err)
	c := &core{events: inputEvents{}, messaging: router}
	input, err := c.prepare(t.Context(), domain.InboxItem{Payload: domain.InboxPayload{Target: "entry:room:", MessageRef: "message"}})
	require.NoError(t, err)
	require.Contains(t, input.Text, "\nrefs: conversation=entry:room:\n\n")
}

func TestInputReactionAndMissingMessageRef(t *testing.T) {
	event := message("room", "member", "reacted")
	event.Kind, event.MessageRef = domain.InboundEventReaction, "message"
	item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event, Target: "ignored", MessageRef: "ignored"}}
	input := formatInput(item, domain.InputContext{LastReply: event.ReceivedAt.Add(time.Second)}, domain.SourceInfo{}, nil, "owner", time.UTC)
	require.Contains(t, input.Text, "in this conversation.\nrefs: conversation=room\n\n")
	require.Contains(t, input.Text, "\nReaction.\n")
	event.MessageRef = ""
	input = formatInput(item, domain.InputContext{}, domain.SourceInfo{}, nil, "owner", time.UTC)
	require.Contains(t, input.Text, "\nrefs: conversation=room\n\n")
}

func TestReadableInputKeepsKnownSenderName(t *testing.T) {
	event := message("room", "B1", "hello")
	event.Sender.DisplayName = "Helpful bot"
	item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event}}
	input := formatInput(item, domain.InputContext{}, domain.SourceInfo{Sender: "B1"}, nil, "owner", time.UTC)
	require.Contains(t, input.Text, "| Helpful bot |")
	require.NotContains(t, input.Text, "B1")
}
