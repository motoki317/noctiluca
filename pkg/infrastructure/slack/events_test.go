package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func TestInboundNotifications(t *testing.T) {
	for _, tc := range []struct {
		name, event, replies, text          string
		accepted, mentionsMe, alsoInChannel bool
		replyCalls, userCalls               int
	}{
		{name: "DM thread", event: `{"type":"message","channel":"DTEST","user":"UMEMBER","ts":"2","thread_ts":"1"}`, accepted: true},
		{name: "DM edit", event: `{"type":"message","subtype":"message_changed","channel":"DTEST","message":{"user":"UMEMBER","ts":"125.000001","text":"edited"}}`, accepted: true, text: "edited"},
		{name: "text and attachments", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"2","text":"Summary","attachments":[{"title":"Alert","from_url":"https://example.test/forwarded","is_share":true,"fallback":"Ignored title summary"}]}`, accepted: true, text: "Summary\nAlert"},
		{name: "bot attachments", event: `{"type":"message","subtype":"bot_message","channel":"CTEST","bot_id":"BOTHER","ts":"2","text":"Summary","attachments":[{},{"title":"Alert","fallback":"Ignored title summary"},{"text":"Service unavailable","fallback":"Ignored text summary"},{"fallback":"Details"},{"original_url":"https://example.test","title":"Preview"},{}]}`, accepted: true, text: "Summary\nAlert\nService unavailable\nDetails"},
		{name: "attachment mention", event: `{"type":"message","subtype":"bot_message","channel":"CTEST","bot_id":"BOTHER","ts":"2","attachments":[{"text":"hi <@USELF>"}]}`, accepted: true, text: "hi <@USELF>"},
		{name: "bot attachment edit", event: `{"type":"message","subtype":"message_changed","channel":"CTEST","message":{"bot_id":"BOTHER","ts":"2","attachments":[{"title":"[RESOLVED]","text":"Service recovered","fallback":"Ignored summary"}]},"previous_message":{"bot_id":"BOTHER","ts":"2","attachments":[{"title":"[FIRING]","text":"Service unavailable"}]}}`, accepted: true, text: "[RESOLVED]\nService recovered"},
		{name: "unfurl attachment", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"2","text":"link","attachments":[{"title":"Preview","text":"Page content","fallback":"Page summary"}]}`, accepted: true, text: "link"},
		{name: "DM reaction on another user", event: `{"type":"reaction_added","channel":"DTEST","user":"UMEMBER","item_user":"UOTHER","item":{"type":"message","channel":"DTEST","ts":"2"}}`, accepted: true, userCalls: 1},
		{name: "root with thread timestamp", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"1","thread_ts":"1"}`, accepted: true},
		{name: "mentioned thread", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"2","thread_ts":"1","text":"hi <@USELF>","files":[{"id":"FTEST","name":"a.png","mimetype":"image/png"}]}`, accepted: true, text: "hi <@USELF>", mentionsMe: true},
		{name: "broadcast reply", event: `{"type":"message","subtype":"thread_broadcast","channel":"CTEST","user":"UMEMBER","ts":"2","thread_ts":"1","text":"hi <@USELF>"}`, accepted: true, text: "hi <@USELF>", mentionsMe: true, alsoInChannel: true},
		{name: "broadcast edit", event: `{"type":"message","subtype":"message_changed","channel":"CTEST","message":{"subtype":"thread_broadcast","user":"UMEMBER","ts":"2","thread_ts":"1","text":"edited <@USELF>"}}`, accepted: true, text: "edited <@USELF>", mentionsMe: true, alsoInChannel: true},
		{name: "broadcast deletion", event: `{"type":"message","subtype":"message_deleted","channel":"CTEST","previous_message":{"subtype":"thread_broadcast","user":"UMEMBER","ts":"2","thread_ts":"1","text":"hi <@USELF>"}}`, accepted: true, alsoInChannel: true},
		{name: "deleted mention", event: `{"type":"message","subtype":"message_deleted","channel":"CTEST","previous_message":{"user":"UMEMBER","ts":"2","thread_ts":"1","text":"hi <@USELF>"}}`, accepted: true},
		{name: "unjoined thread", event: `{"type":"message","channel":"CTEST","user":"UMEMBER","ts":"2","thread_ts":"1"}`, replies: `{"ok":true,"messages":[{"user":"UOTHER","ts":"1"}]}`, replyCalls: 1},
		{name: "unjoined edit", event: `{"type":"message","subtype":"message_changed","channel":"CTEST","message":{"user":"UMEMBER","ts":"2","thread_ts":"1","text":"edited"}}`, replies: `{"ok":true,"messages":[]}`, replyCalls: 1},
		{name: "unruled channel", event: `{"type":"message","channel":"CDENIED","user":"UMEMBER","ts":"2","thread_ts":"1"}`, accepted: true},
		{name: "reaction on agent", event: `{"type":"reaction_added","user":"UMEMBER","item_user":"USELF","item":{"type":"message","channel":"CTEST","ts":"2"}}`, accepted: true, userCalls: 1},
		{name: "reaction on another user", event: `{"type":"reaction_added","user":"UMEMBER","item_user":"UOTHER","item":{"type":"message","channel":"CTEST","ts":"2"}}`},
		{name: "own reaction", event: `{"type":"reaction_added","user":"USELF","item_user":"USELF","item":{"type":"message","channel":"CTEST","ts":"2"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				return ref == Conversation("slack", "CTEST", "") || ref == Conversation("slack", "DTEST", "")
			})
			parsed, err := slackevents.ParseEvent(json.RawMessage(`{"type":"event_callback","event":`+tc.event+`}`), slackevents.OptionNoVerifyToken())
			require.NoError(t, err)
			event, accepted := a.inbound(t.Context(), parsed.InnerEvent.Data)
			require.Equal(t, tc.accepted, accepted)
			require.Equal(t, tc.replyCalls, replyCalls)
			require.Equal(t, tc.userCalls, userCalls)
			if accepted && event.Kind != domain.InboundEventReaction {
				require.Equal(t, tc.text, event.Text)
				require.Equal(t, tc.mentionsMe, event.MentionsMe)
				require.Equal(t, tc.alsoInChannel, event.AlsoInChannel)
			}
			if tc.name == "mentioned thread" {
				require.Equal(t, []domain.Attachment{{Ref: "FTEST", Name: "a.png", MIMEType: "image/png"}}, event.Attachments)
			}
			if tc.name == "DM edit" {
				require.Equal(t, domain.InboundEventMessageEdited, event.Kind)
				require.Equal(t, Conversation("slack", "DTEST", ""), event.Conversation)
			}
			if tc.name == "reaction on agent" {
				require.Equal(t, domain.SenderHuman, event.Sender.Kind)
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
		{"bot reply", &slackevents.MessageEvent{BotID: "BSELF", SubType: "bot_message", TimeStamp: "2", ThreadTimeStamp: "1"}},
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
	synctest.Test(t, func(t *testing.T) {
		bot := sdk.New("xoxb-test", sdk.OptionAPIURL("https://example.test/"),
			sdk.OptionHTTPClient(&http.Client{Transport: uploadTransport(func(_ http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/conversations.replies", r.URL.Path)
				<-r.Context().Done()
			})}))
		a := &adapter{name: "slack", selfUser: "USELF", bot: bot, reader: bot}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		_, accepted := a.inbound(ctx, &slackevents.MessageEvent{Channel: "CTEST", User: "UMEMBER", TimeStamp: "2", ThreadTimeStamp: "1"})
		require.True(t, accepted)
		require.NoError(t, ctx.Err())
	})
}
