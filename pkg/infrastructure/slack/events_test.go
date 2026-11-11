package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	sdk "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func TestInboundMessageSourceKeys(t *testing.T) {
	for _, subtype := range []string{"", "bot_message", "file_share", "me_message", "thread_broadcast"} {
		t.Run(subtype, func(t *testing.T) {
			a := apiAdapter(t, func(_ http.ResponseWriter, r *http.Request) { t.Errorf("unexpected API %s", r.URL.Path) }, false)
			event, accepted := a.inbound(t.Context(), &slackevents.MessageEvent{
				SubType: subtype, Channel: "D1", User: "U1", TimeStamp: "2.3", ThreadTimeStamp: "1.2",
			})
			require.True(t, accepted)
			require.Equal(t, "slack:m:D1:2.3", event.SourceKey)
			require.Equal(t, Conversation("slack", "D1", "1.2"), event.Conversation)
			require.Equal(t, Conversation("slack", "D1", "2.3"), event.MessageRef)
		})
	}
}

func TestInboundNotifications(t *testing.T) {
	for _, tc := range []struct {
		name, event, replies  string
		accepted              bool
		replyCalls, userCalls int
		warning               bool
	}{
		{name: "DM", event: `{"type":"message","channel":"DTEST","user":"UMEMBER","ts":"2"}`, accepted: true},
		{name: "DM thread", event: `{"type":"message","channel":"DTEST","user":"UMEMBER","ts":"2","thread_ts":"1"}`, accepted: true},
		{name: "DM edit", event: `{"type":"message","subtype":"message_changed","channel":"DTEST","message":{"user":"UMEMBER","ts":"2","thread_ts":"1","text":"edited"}}`, accepted: true},
		{name: "DM delete", event: `{"type":"message","subtype":"message_deleted","channel":"DTEST","previous_message":{"user":"UMEMBER","ts":"2","thread_ts":"1"}}`, accepted: true},
		{name: "DM reaction on another user", event: `{"type":"reaction_added","channel":"DTEST","user":"UMEMBER","item_user":"UOTHER","item":{"type":"message","channel":"DTEST","ts":"2"}}`, accepted: true, userCalls: 1},
		{name: "top level", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"1"}`, accepted: true},
		{name: "root with thread timestamp", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"1","thread_ts":"1"}`, accepted: true},
		{name: "top level edit", event: `{"type":"message","subtype":"message_changed","channel":"CTEST","message":{"user":"UMEMBER","ts":"1","thread_ts":"1","text":"edited"}}`, accepted: true},
		{name: "top level delete", event: `{"type":"message","subtype":"message_deleted","channel":"CTEST","previous_message":{"user":"UMEMBER","ts":"1"}}`, accepted: true},
		{name: "mentioned thread", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"2","thread_ts":"1","text":"hi <@USELF>"}`, accepted: true},
		{name: "mentioned edit", event: `{"type":"message","subtype":"message_changed","channel":"CTEST","message":{"user":"UMEMBER","ts":"2","thread_ts":"1","text":"hi <@USELF>"}}`, accepted: true},
		{name: "deleted mention", event: `{"type":"message","subtype":"message_deleted","channel":"CTEST","previous_message":{"user":"UMEMBER","ts":"2","thread_ts":"1","text":"hi <@USELF>"}}`, accepted: true},
		{name: "agent root", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"2","thread_ts":"1"}`, replies: `{"ok":true,"messages":[{"user":"USELF","ts":"1"}]}`, accepted: true, replyCalls: 1},
		{name: "agent reply", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"3","thread_ts":"1"}`, replies: `{"ok":true,"messages":[{"user":"UOTHER","ts":"1"},{"user":"USELF","ts":"2"}]}`, accepted: true, replyCalls: 1},
		{name: "agent bot ID", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"3","thread_ts":"1"}`, replies: `{"ok":true,"messages":[{"bot_id":"BSELF","ts":"1"}]}`, accepted: true, replyCalls: 1},
		{name: "unjoined thread", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"2","thread_ts":"1"}`, replies: `{"ok":true,"messages":[{"user":"UOTHER","ts":"1"}]}`, replyCalls: 1},
		{name: "unjoined private channel thread", event: `{"type":"message","channel":"GTEST","user":"UMEMBER","ts":"2","thread_ts":"1"}`, replies: `{"ok":true,"messages":[]}`, replyCalls: 1},
		{name: "joined edit", event: `{"type":"message","subtype":"message_changed","channel":"CTEST","message":{"user":"UMEMBER","ts":"2","thread_ts":"1","text":"edited"}}`, replies: `{"ok":true,"messages":[{"user":"USELF","ts":"1"}]}`, accepted: true, replyCalls: 1},
		{name: "unjoined edit", event: `{"type":"message","subtype":"message_changed","channel":"CTEST","message":{"user":"UMEMBER","ts":"2","thread_ts":"1","text":"edited"}}`, replies: `{"ok":true,"messages":[]}`, replyCalls: 1},
		{name: "joined delete", event: `{"type":"message","subtype":"message_deleted","channel":"CTEST","previous_message":{"user":"UMEMBER","ts":"2","thread_ts":"1"}}`, replies: `{"ok":true,"messages":[{"user":"USELF","ts":"1"}]}`, accepted: true, replyCalls: 1},
		{name: "unjoined delete", event: `{"type":"message","subtype":"message_deleted","channel":"CTEST","previous_message":{"user":"UMEMBER","ts":"2","thread_ts":"1"}}`, replies: `{"ok":true,"messages":[]}`, replyCalls: 1},
		{name: "joined broadcast", event: `{"type":"message","subtype":"thread_broadcast","channel":"CTEST","user":"UMEMBER","ts":"2","thread_ts":"1"}`, replies: `{"ok":true,"messages":[{"user":"USELF","ts":"1"}]}`, accepted: true, replyCalls: 1},
		{name: "unjoined broadcast", event: `{"type":"message","subtype":"thread_broadcast","channel":"CTEST","user":"UMEMBER","ts":"2","thread_ts":"1"}`, replies: `{"ok":true,"messages":[]}`, replyCalls: 1},
		{name: "lookup failure", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"2","thread_ts":"1"}`, replies: `{"ok":false,"error":"missing_scope"}`, accepted: true, replyCalls: 1, warning: true},
		{name: "unruled channel", event: `{"type":"message","channel":"CDENIED","user":"UMEMBER","ts":"2","thread_ts":"1"}`, accepted: true},
		{name: "reaction on agent", event: `{"type":"reaction_added","user":"UMEMBER","item_user":"USELF","item":{"type":"message","channel":"CTEST","ts":"2"}}`, accepted: true, userCalls: 1},
		{name: "reaction on another user", event: `{"type":"reaction_added","user":"UMEMBER","item_user":"UOTHER","item":{"type":"message","channel":"CTEST","ts":"2"}}`},
		{name: "reaction without target user", event: `{"type":"reaction_added","user":"UMEMBER","item":{"type":"message","channel":"CTEST","ts":"2"}}`},
		{name: "own reaction", event: `{"type":"reaction_added","user":"USELF","item_user":"USELF","item":{"type":"message","channel":"CTEST","ts":"2"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(logger) })
			var replyCalls, userCalls int
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				require.Equal(t, "xoxb-test", r.Form.Get("token"))
				switch r.URL.Path {
				case "/conversations.replies":
					replyCalls++
					require.Equal(t, "1", r.Form.Get("ts"))
					_, _ = w.Write([]byte(tc.replies))
				case "/users.info":
					userCalls++
					_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"UMEMBER","is_bot":false}}`))
				default:
					t.Errorf("unexpected API %s", r.URL.Path)
				}
			}, false)
			a.selfUser, a.selfBot = "USELF", "BSELF"
			a.SetConversationFilter(func(ref domain.ConversationRef) bool {
				return ref == Conversation("slack", "CTEST", "") || ref == Conversation("slack", "GTEST", "") || ref == Conversation("slack", "DTEST", "")
			})
			parsed, err := slackevents.ParseEvent(json.RawMessage(`{"type":"event_callback","event":`+tc.event+`}`), slackevents.OptionNoVerifyToken())
			require.NoError(t, err)
			event, accepted := a.inbound(t.Context(), parsed.InnerEvent.Data)
			require.Equal(t, tc.accepted, accepted)
			require.Equal(t, tc.replyCalls, replyCalls)
			require.Equal(t, tc.userCalls, userCalls)
			if tc.warning {
				require.Contains(t, logs.String(), "level=WARN")
				require.Contains(t, logs.String(), "missing_scope")
			} else {
				require.Empty(t, logs.String())
			}
			if event.Deleted {
				require.Empty(t, event.Text)
				require.False(t, event.MentionsMe)
			}
		})
	}
}

func TestThreadParticipationCache(t *testing.T) {
	type request struct{ token, cursor, response string }
	type outcome struct {
		accepted bool
		calls    int
	}
	for _, tc := range []struct {
		name     string
		requests []request
		outcomes []outcome
	}{
		{
			name:     "stop at agent root",
			requests: []request{{"xoxb-test", "", `{"ok":true,"messages":[{"user":"USELF","ts":"1"}],"has_more":true,"response_metadata":{"next_cursor":"unused"}}`}},
			outcomes: []outcome{{true, 1}, {true, 1}},
		},
		{
			name: "agent on later page",
			requests: []request{
				{"xoxb-test", "", `{"ok":true,"messages":[{"user":"UOTHER","ts":"1"}],"has_more":true,"response_metadata":{"next_cursor":"next"}}`},
				{"xoxb-test", "next", `{"ok":true,"messages":[{"bot_id":"BSELF","ts":"2"}],"has_more":true,"response_metadata":{"next_cursor":"unused"}}`},
			},
			outcomes: []outcome{{true, 2}, {true, 2}},
		},
		{
			name: "negative result is not cached",
			requests: []request{
				{"xoxb-test", "", `{"ok":true,"messages":[{"user":"UOTHER","ts":"1"}],"has_more":true,"response_metadata":{"next_cursor":"next"}}`},
				{"xoxb-test", "next", `{"ok":true,"messages":[{"user":"UOTHER","ts":"2"}]}`},
				{"xoxb-test", "", `{"ok":true,"messages":[{"user":"USELF","ts":"3"}]}`},
			},
			outcomes: []outcome{{false, 2}, {true, 3}, {true, 3}},
		},
		{
			name: "error is not cached",
			requests: []request{
				{"xoxb-test", "", `{"ok":false,"error":"missing_scope"}`},
				{"xoxb-test", "", `{"ok":true,"messages":[]}`},
			},
			outcomes: []outcome{{true, 1}, {false, 2}},
		},
		{
			name: "error on later page fails open",
			requests: []request{
				{"xoxb-test", "", `{"ok":true,"messages":[],"has_more":true,"response_metadata":{"next_cursor":"next"}}`},
				{"xoxb-test", "next", `{"ok":false,"error":"invalid_auth"}`},
				{"xoxb-test", "", `{"ok":true,"messages":[{"user":"USELF","ts":"1"}]}`},
			},
			outcomes: []outcome{{true, 2}, {true, 3}, {true, 3}},
		},
		{
			name: "user fallback keeps pagination",
			requests: []request{
				{"xoxb-test", "", `{"ok":false,"error":"not_in_channel"}`},
				{"xoxp-test", "", `{"ok":true,"messages":[{"user":"UOTHER","ts":"1"}],"has_more":true,"response_metadata":{"next_cursor":"next"}}`},
				{"xoxp-test", "next", `{"ok":true,"messages":[{"user":"USELF","ts":"2"}]}`},
			},
			outcomes: []outcome{{true, 3}, {true, 3}},
		},
		{
			name: "user fallback failure is not retried or cached",
			requests: []request{
				{"xoxb-test", "", `{"ok":false,"error":"channel_not_found"}`},
				{"xoxp-test", "", `{"ok":false,"error":"not_in_channel"}`},
				{"xoxb-test", "", `{"ok":true,"messages":[]}`},
			},
			outcomes: []outcome{{true, 2}, {false, 3}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				require.Equal(t, "/conversations.replies", r.URL.Path)
				require.Less(t, calls, len(tc.requests))
				step := tc.requests[calls]
				calls++
				require.Equal(t, step.token, r.Form.Get("token"))
				require.Equal(t, step.cursor, r.Form.Get("cursor"))
				require.Equal(t, "CTEST", r.Form.Get("channel"))
				require.Equal(t, "1", r.Form.Get("ts"))
				_, _ = w.Write([]byte(step.response))
			}, true)
			a.selfUser, a.selfBot = "USELF", "BSELF"
			for _, want := range tc.outcomes {
				_, accepted := a.inbound(t.Context(), &slackevents.MessageEvent{Channel: "CTEST", User: "UMEMBER", TimeStamp: "4", ThreadTimeStamp: "1"})
				require.Equal(t, want.accepted, accepted)
				require.Equal(t, want.calls, calls)
			}
		})
	}
}

func TestOwnMessagesMarkThreads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event *slackevents.MessageEvent
	}{
		{"user root", &slackevents.MessageEvent{User: "USELF", TimeStamp: "1"}},
		{"bot root", &slackevents.MessageEvent{BotID: "BSELF", SubType: "bot_message", TimeStamp: "1"}},
		{"user reply", &slackevents.MessageEvent{User: "USELF", TimeStamp: "2", ThreadTimeStamp: "1"}},
		{"bot reply", &slackevents.MessageEvent{BotID: "BSELF", SubType: "bot_message", TimeStamp: "2", ThreadTimeStamp: "1"}},
		{"root thread timestamp", &slackevents.MessageEvent{User: "USELF", TimeStamp: "1", ThreadTimeStamp: "1"}},
		{"own unfurl", &slackevents.MessageEvent{SubType: "message_changed", Message: &sdk.Msg{User: "USELF", Timestamp: "2", ThreadTimestamp: "1"}, PreviousMessage: &sdk.Msg{User: "USELF", Timestamp: "2", ThreadTimestamp: "1"}}},
		{"own delete", &slackevents.MessageEvent{SubType: "message_deleted", PreviousMessage: &sdk.Msg{User: "USELF", Timestamp: "2", ThreadTimestamp: "1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := apiAdapter(t, func(_ http.ResponseWriter, r *http.Request) { t.Errorf("unexpected API %s", r.URL.Path) }, false)
			a.selfUser, a.selfBot = "USELF", "BSELF"
			tc.event.Channel = "CTEST"
			_, accepted := a.inbound(t.Context(), tc.event)
			require.False(t, accepted)
			_, accepted = a.inbound(t.Context(), &slackevents.MessageEvent{Channel: "CTEST", User: "UMEMBER", TimeStamp: "3", ThreadTimeStamp: "1"})
			require.True(t, accepted)
		})
	}
}

func TestThreadCacheScope(t *testing.T) {
	calls := 0
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/conversations.replies", r.URL.Path)
		calls++
		_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
	}, false)
	a.selfUser = "USELF"
	_, accepted := a.inbound(t.Context(), &slackevents.MessageEvent{Channel: "CTEST", User: "USELF", TimeStamp: "1"})
	require.False(t, accepted)
	for _, tc := range []struct {
		channel, thread string
		accepted        bool
		calls           int
	}{
		{"CTEST", "1", true, 0},
		{"COTHER", "1", false, 1},
		{"CTEST", "2", false, 2},
	} {
		_, accepted := a.inbound(t.Context(), &slackevents.MessageEvent{Channel: tc.channel, User: "UMEMBER", TimeStamp: "3", ThreadTimeStamp: tc.thread})
		require.Equal(t, tc.accepted, accepted)
		require.Equal(t, tc.calls, calls)
	}
}

func TestThreadLookupTimeoutFailsOpen(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(logger) })
	a := apiAdapter(t, func(_ http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/conversations.replies", r.URL.Path)
		require.NoError(t, r.ParseForm())
		<-r.Context().Done()
	}, false)
	a.selfUser = "USELF"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, accepted := a.inbound(ctx, &slackevents.MessageEvent{Channel: "CTEST", User: "UMEMBER", TimeStamp: "2", ThreadTimeStamp: "1"})
	require.True(t, accepted)
	require.NoError(t, ctx.Err())
	require.Contains(t, logs.String(), "level=WARN")
	require.Contains(t, logs.String(), "context deadline exceeded")
	_, accepted = a.inbound(ctx, &slackevents.MessageEvent{Channel: "DTEST", User: "UMEMBER", TimeStamp: "3"})
	require.True(t, accepted)
}
