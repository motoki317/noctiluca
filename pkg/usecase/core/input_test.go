package core

import (
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
		{"first thread", domain.InputContext{}, "#general · thread 16:28:01 JST \"What did you run?\" | Alex (owner) | 16:29:01 JST\nreply → thread | work → thread\n\nadd 了解 at the end"},
		{"before reply", domain.InputContext{ThreadSeen: true, Running: true, LastReply: at.Add(time.Second)}, "#general · thread 16:28:01 JST | Alex (owner) | 16:29:01 JST\nSent at 16:29:01 JST, before your reply at 16:29:02 JST in this thread.\nreply → thread | work → thread\n\nadd 了解 at the end"},
		{"queued", domain.InputContext{ThreadSeen: true, Running: true}, "#general · thread 16:28:01 JST | Alex (owner) | 16:29:01 JST\nSent while your previous turn was running. After its reply, answer only what this message adds.\nreply → thread | work → thread\n\nadd 了解 at the end"},
	} {
		t.Run(tt.name, func(t *testing.T) { require.Equal(t, tt.want, formatInput(item, tt.history, info, "owner", zone).Text) })
	}
	event.Parent, event.Conversation, event.Direct = "", "room", true
	require.Equal(t, "DM with Alex · top level | Alex (owner) | 16:29:01 JST\nreply → room | work → room\n\nadd 了解 at the end", formatInput(item, domain.InputContext{}, domain.SourceInfo{Conversation: "Alex", Sender: "Alex"}, "owner", zone).Text)
	item.Payload.Event = nil
	item.Payload.Context = domain.SessionContext{Conversation: "room"}
	item.Payload.Target, item.ReadyAt, item.SourceKey = "thread", at, "watch:1"
	item.Payload.Input.Text = "exit code 0: DONE"
	require.Equal(t, "#general · thread 16:28:01 JST | watch | 16:29:01 JST\nreply → thread | work → thread\n\nexit code 0: DONE", formatInput(item, domain.InputContext{ThreadSeen: true}, info, "owner", zone).Text)
	item.SourceKey = "wake:1"
	require.Contains(t, formatInput(item, domain.InputContext{}, info, "owner", zone).Text, "| wake |")
	item.Lane, item.Payload.Context.TaskName = "task", "heartbeat"
	require.Equal(t, "Task heartbeat | wake | 16:29:01 JST\nreply → owner | work → owner\n\nexit code 0: DONE", formatInput(item, domain.InputContext{}, info, "owner", zone).Text)
	require.Equal(t, strings.Repeat("あ", 80)+"…", excerpt(strings.Repeat("あ", 81)))
}

func TestWorkTargetSurvivesChatToContinuation(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, threaded := range []bool{false, true} {
			t.Run(fmt.Sprintf("direct=%t/threaded=%t", direct, threaded), func(t *testing.T) {
				event := message("room", "member", "start work")
				event.Direct, event.MessageRef = direct, "message-thread"
				target := domain.ConversationRef("message-thread")
				if direct {
					target = "room"
				}
				if threaded {
					event.Parent, event.Conversation, target = "room", "existing-thread", "existing-thread"
				}
				item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event, Context: domain.SessionContext{Conversation: "room", Direct: direct}}}
				input := formatInput(item, domain.InputContext{}, domain.SourceInfo{}, "owner", time.UTC)
				require.Contains(t, input.Text, fmt.Sprintf("reply → %s | work → %s", event.Conversation, target))
				for _, key := range []string{"watch:1", "wake:1"} {
					item.SourceKey, item.Payload.Target, item.Payload.Event = key, target, nil
					input = formatInput(item, domain.InputContext{}, domain.SourceInfo{}, "owner", time.UTC)
					require.Contains(t, input.Text, fmt.Sprintf("reply → %s | work → %s", target, target))
				}
			})
		}
	}
}

func TestReadableInputKeepsKnownSenderName(t *testing.T) {
	event := message("room", "B1", "hello")
	event.Sender.DisplayName = "Helpful bot"
	item := domain.InboxItem{Payload: domain.InboxPayload{Event: &event}}
	input := formatInput(item, domain.InputContext{}, domain.SourceInfo{Sender: "B1"}, "owner", time.UTC)
	require.Contains(t, input.Text, "| Helpful bot |")
	require.NotContains(t, input.Text, "B1")
}
