package slack

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	sdk "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/require"
)

func TestSourceInfoNamesAndRoot(t *testing.T) {
	calls := map[string]int{}
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		calls[r.URL.Path]++
		switch r.URL.Path {
		case "/users.info":
			fmt.Fprint(w, `{"ok":true,"user":{"profile":{"display_name":"Alex"}}}`)
		case "/conversations.info":
			if r.Form.Get("channel") == "D1" {
				fmt.Fprint(w, `{"ok":true,"channel":{"is_im":true,"user":"U1"}}`)
			} else {
				fmt.Fprint(w, `{"ok":true,"channel":{"name":"general"}}`)
			}
		case "/conversations.replies":
			fmt.Fprint(w, `{"ok":true,"messages":[{"ts":"1730000000.123456","text":"Root question"}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}, false)
	info := a.SourceInfo(t.Context(), Conversation("slack", "C1", "1730000000.123456"), "U1", true)
	require.Equal(t, "#general", info.Conversation)
	require.Equal(t, "Alex", info.Sender)
	require.Equal(t, "Root question", info.ThreadText)
	require.Equal(t, time.Unix(1730000000, 123456000), info.ThreadAt)
	a.SourceInfo(t.Context(), Conversation("slack", "C1", "1730000000.123456"), "U1", false)
	require.Equal(t, 1, calls["/users.info"])
	require.Equal(t, 1, calls["/conversations.info"])
	require.Equal(t, 1, calls["/conversations.replies"])
	require.Equal(t, "Alex", a.SourceInfo(t.Context(), Conversation("slack", "D1", ""), "U1", false).Conversation)
}

func TestSourceInfoUnavailableFallsBackToIDs(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ok":false,"error":"missing_scope"}`)
	}, false)
	info := a.SourceInfo(t.Context(), Conversation("slack", "C1", "1.2"), "U1", true)
	require.Equal(t, "C1", info.Conversation)
	require.Equal(t, "U1", info.Sender)
	require.Empty(t, info.ThreadText)
}

func TestInboundUsesPostedTime(t *testing.T) {
	a := &adapter{name: "slack"}
	for _, tt := range []struct {
		name  string
		event *slackevents.MessageEvent
		want  time.Time
	}{
		{"message", &slackevents.MessageEvent{Channel: "C1", User: "U1", TimeStamp: "1730000000.123456"}, time.Unix(1730000000, 123456000)},
		{"edit", &slackevents.MessageEvent{Channel: "C1", SubType: "message_changed", Message: &sdk.Msg{User: "U1", Timestamp: "1.0", Edited: &sdk.Edited{Timestamp: "1730000001.0"}}}, time.Unix(1730000001, 0)},
		{"delete", &slackevents.MessageEvent{Channel: "C1", SubType: "message_deleted", EventTimeStamp: "1730000002.0", PreviousMessage: &sdk.Msg{User: "U1", Timestamp: "1.0"}}, time.Unix(1730000002, 0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			event, ok := a.inbound(t.Context(), tt.event)
			require.True(t, ok)
			require.Equal(t, tt.want, event.ReceivedAt)
		})
	}
}

func TestSourceInfoRetriesFailedLookups(t *testing.T) {
	healthy := false
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if !healthy {
			fmt.Fprint(w, `{"ok":false,"error":"ratelimited"}`)
			return
		}
		if r.URL.Path == "/users.info" {
			fmt.Fprint(w, `{"ok":true,"user":{"profile":{"display_name":"Alex"}}}`)
		} else {
			fmt.Fprint(w, `{"ok":true,"channel":{"name":"general"}}`)
		}
	}, false)
	ref := Conversation("slack", "C1", "")
	info := a.SourceInfo(t.Context(), ref, "U1", false)
	require.Equal(t, "U1", info.Sender)
	require.Equal(t, "C1", info.Conversation)
	healthy = true
	info = a.SourceInfo(t.Context(), ref, "U1", false)
	require.Equal(t, "Alex", info.Sender)
	require.Equal(t, "#general", info.Conversation)
}

func TestReactionUsesPostedTime(t *testing.T) {
	a := &adapter{name: "slack", selfUser: "USELF", acceptConversation: func(_ domain.ConversationRef, _ bool) bool { return false }}
	event, ok := a.inbound(t.Context(), &slackevents.ReactionAddedEvent{User: "U1", ItemUser: "USELF", EventTimestamp: "1730000003.000001", Item: slackevents.Item{Type: "message", Channel: "C1", Timestamp: "1.0"}})
	require.True(t, ok)
	require.Equal(t, time.Unix(1730000003, 1000), event.ReceivedAt)
}

func TestSourceInfoRetriesDMPeerName(t *testing.T) {
	healthy := false
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/conversations.info" {
			fmt.Fprint(w, `{"ok":true,"channel":{"is_im":true,"user":"U1"}}`)
			return
		}
		if healthy {
			fmt.Fprint(w, `{"ok":true,"user":{"profile":{"display_name":"Alex"}}}`)
		} else {
			fmt.Fprint(w, `{"ok":false,"error":"ratelimited"}`)
		}
	}, false)
	ref := Conversation("slack", "D1", "")
	require.Equal(t, "U1", a.SourceInfo(t.Context(), ref, "U1", false).Conversation)
	healthy = true
	require.Equal(t, "Alex", a.SourceInfo(t.Context(), ref, "U1", false).Conversation)
}
