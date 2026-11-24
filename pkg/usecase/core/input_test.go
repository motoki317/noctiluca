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
	item := domain.InboxItem{Lane: domain.LaneConversation, Payload: domain.InboxPayload{Event: &event}}
	info := domain.SourceInfo{Location: "entry: native IDs", Conversation: "#general", Sender: "Alex", ThreadAt: at.Add(-time.Minute), ThreadText: "What did\nyou run?"}
	noHistoryText := formatInput(item, domain.InputContext{}, info, "owner", zone, "").Text
	require.Equal(t, "#general · thread 16:28:01 JST \"What did you run?\" | Alex (owner) | 16:29:01 JST\nentry: native IDs\n\nadd 了解 at the end", noHistoryText)
	input := formatInput(item, domain.InputContext{Running: true}, info, "owner", zone, "")
	require.Equal(t, noHistoryText, input.Text)
	require.True(t, input.SentDuringTurn)
	for _, tt := range []struct {
		name    string
		history domain.InputContext
		want    string
	}{
		{"deleted root", domain.InputContext{Root: &domain.InboundEvent{Deleted: true}}, "(root deleted)"},
		{"before reply", domain.InputContext{Running: true, LastReply: at.Add(time.Second)}, "Sent at 16:29:01 JST, before your reply at 16:29:02 JST in this thread."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := formatInput(item, tt.history, info, "owner", zone, "")
			require.Contains(t, input.Text, tt.want)
			require.False(t, input.SentDuringTurn)
			if tt.history.Root == nil {
				require.Contains(t, input.Text, `"What did you run?"`)
			}
		})
	}
	event.AlsoInChannel = true
	require.Contains(t, formatInput(item, domain.InputContext{}, info, "owner", zone, "").Text, `"What did you run?" (also sent to channel) | `)
	event.Parent, event.Conversation, event.Direct = "", "room", true
	require.Contains(t, formatInput(item, domain.InputContext{}, domain.SourceInfo{Conversation: "Alex", Sender: "Alex"}, "owner", zone, "").Text, "DM with Alex · top level | ")
	item.Payload.Event = nil
	item.Lane, item.Payload.Context.TaskName, item.ReadyAt = domain.LaneTask, domain.HeartbeatSchedule, at
	require.Equal(t, "Task heartbeat | system | 16:29:01 JST\n\n", formatInput(item, domain.InputContext{}, info, "owner", zone, "").Text)
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

func TestExternalEventInput(t *testing.T) {
	at := time.Date(2026, 11, 24, 6, 4, 5, 0, time.UTC)
	zone := time.FixedZone("JST", 9*60*60)
	item := domain.InboxItem{
		ReadyAt: at,
		Payload: domain.InboxPayload{
			External: &domain.ExternalEvent{Name: "calendar"},
			Input:    domain.UserInput{Text: " body\n"},
		},
	}
	const header = "Event calendar | external | 15:04:05 JST\n"
	require.Equal(t, header+"\n body\n", formatInput(item, domain.InputContext{}, domain.SourceInfo{}, "", zone, "").Text)
	item.Payload.External.Truncated = true
	require.Equal(t, header+"The body was cut. Read the rest from its source.\n\n body\n", formatInput(item, domain.InputContext{}, domain.SourceInfo{}, "", zone, "").Text)
}

type rootMessaging struct {
	*namedMessaging
	fetchRoot bool
}

func (m *rootMessaging) SourceInfo(_ context.Context, _ domain.InboundEvent, root bool) domain.SourceInfo {
	m.fetchRoot = root
	return domain.SourceInfo{ThreadText: "Fetched root"}
}

func TestPrepareIncludesRootForEveryInput(t *testing.T) {
	for _, root := range []*domain.InboundEvent{nil, {Text: "Stored root"}} {
		adapter := &rootMessaging{namedMessaging: newNamedMessaging("test")}
		router, err := newMessagingRouter([]Messaging{{Name: "test", Adapter: adapter, Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Users: []string{"owner"}}}}})
		require.NoError(t, err)
		c := &core{events: inputEvents{history: domain.InputContext{Root: root}}, messaging: router}
		event := message("test:thread", "owner", "reply")
		event.Parent = "test:room"
		for range 2 {
			input, err := c.prepare(t.Context(), domain.InboxItem{Payload: domain.InboxPayload{Event: &event}})
			require.NoError(t, err)
			require.Equal(t, root == nil, adapter.fetchRoot)
			if root == nil {
				require.Contains(t, input.Text, `"Fetched root"`)
			} else {
				require.Contains(t, input.Text, `"Stored root"`)
			}
		}
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
			input := formatInput(item, domain.InputContext{}, domain.SourceInfo{}, "owner", time.UTC, "")
			require.Contains(t, input.Text, "\n"+tc.notice+"\n")
		})
	}
}

type inputEvents struct {
	domain.EventRepository
	key     *domain.SessionKey
	history domain.InputContext
}

func (e inputEvents) InputContext(_ context.Context, key domain.SessionKey, _ domain.ConversationRef, _ time.Time) (domain.InputContext, error) {
	if e.key != nil {
		*e.key = key
	}
	return e.history, nil
}

func TestPrepareMessagingEventWithBackgroundPrefix(t *testing.T) {
	adapter := newNamedMessaging("watch")
	router, err := newMessagingRouter([]Messaging{{Name: "watch", Adapter: adapter, Rules: map[domain.ConversationRef]domain.AcceptRule{"watch:room": {Users: []string{"owner"}}}}})
	require.NoError(t, err)
	var key domain.SessionKey
	c := &core{events: inputEvents{key: &key, history: domain.InputContext{Running: true}}, messaging: router}
	event := message("watch:room", "owner", "hello")
	input, err := c.prepare(t.Context(), domain.InboxItem{SourceKey: "watch:message", SessionKey: "old-key", Payload: domain.InboxPayload{Event: &event}})
	require.NoError(t, err)
	require.Contains(t, input.Text, "hello")
	require.Contains(t, input.Text, "watch user")
	require.Equal(t, domain.MainSessionKey, key)
	require.True(t, input.SentDuringTurn)
	require.NotContains(t, input.Text, "answer only")
}

func TestInputReaction(t *testing.T) {
	event := message("room", "member", "reacted")
	event.Kind, event.MessageRef = domain.InboundEventReaction, "entry:channel:message"
	item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event}}
	input := formatInput(item, domain.InputContext{LastReply: event.ReceivedAt.Add(time.Second)}, domain.SourceInfo{}, "owner", time.UTC, "")
	require.Contains(t, input.Text, "in this conversation.\n\n")
	require.Contains(t, input.Text, "\nReaction.\n")
	require.NotContains(t, input.Text, "\nmessage:")
}

func TestReadableInputKeepsKnownSenderName(t *testing.T) {
	event := message("room", "B1", "hello")
	event.Sender.DisplayName = "Helpful bot"
	item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event}}
	input := formatInput(item, domain.InputContext{}, domain.SourceInfo{Sender: "B1"}, "owner", time.UTC, "")
	require.Contains(t, input.Text, "| Helpful bot |")
	require.NotContains(t, input.Text, "B1")
}

func TestTaskWorkdirHint(t *testing.T) {
	for _, workdir := range []string{"", "/default", "/project"} {
		t.Run(workdir, func(t *testing.T) {
			c := &core{config: Config{Workdir: "/default"}}
			item := domain.InboxItem{Lane: domain.LaneTask, Payload: domain.InboxPayload{Context: domain.SessionContext{TaskName: "daily", Workdir: workdir}, Input: domain.UserInput{Text: "work"}}}
			input, err := c.prepare(t.Context(), item)
			require.NoError(t, err)
			require.Contains(t, input.Text, "Task daily | system |")
			if workdir == "/project" {
				require.Contains(t, input.Text, "\nworkdir: /project\n\nwork")
			} else {
				require.NotContains(t, input.Text, "workdir:")
			}
			require.NotContains(t, input.Text, "refs:")
		})
	}
}
