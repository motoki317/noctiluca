package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type unusedHistory struct{ domain.EventRepository }

func apiAdapter(t *testing.T, handler http.HandlerFunc, user bool) *adapter {
	t.Helper()
	return apiInstance(t, "slack", handler, user)
}

func apiInstance(t *testing.T, name string, handler http.HandlerFunc, user bool) *adapter {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	config := Config{AppToken: "xapp-test", BotToken: "xoxb-test"}
	if user {
		config.UserToken = "xoxp-test"
	}
	a, err := newAdapter(name, config, &unusedHistory{}, sdk.OptionAPIURL(server.URL+"/"), sdk.OptionHTTPClient(server.Client()))
	require.NoError(t, err)
	return a
}

func TestSendAndGroups(t *testing.T) {
	for _, user := range []bool{false, true} {
		t.Run(map[bool]string{false: "bot reads", true: "user reads"}[user], func(t *testing.T) {
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				switch r.URL.Path {
				case "/files.info":
					want := "xoxb-test"
					if user {
						want = "xoxp-test"
					}
					require.Equal(t, want, r.Form.Get("token"))
					_, _ = w.Write([]byte(`{"ok":true,"file":{"id":"FTEST","name":"image.png","permalink":"https://example.test/file"}}`))
				case "/chat.postMessage":
					require.Equal(t, "xoxb-test", r.Form.Get("token"))
					require.Equal(t, "CTEST", r.Form.Get("channel"))
					require.Equal(t, "123.000001", r.Form.Get("thread_ts"))
					require.Contains(t, r.Form.Get("text"), "*bold* <https://example.test|link>")
					require.Contains(t, r.Form.Get("text"), "<https://example.test/file|image.png>")
					_, _ = w.Write([]byte(`{"ok":true,"channel":"CTEST","ts":"124.000001"}`))
				case "/usergroups.users.list":
					require.Equal(t, "xoxb-test", r.Form.Get("token"))
					require.Equal(t, "GTEST", r.Form.Get("usergroup"))
					_, _ = w.Write([]byte(`{"ok":true,"users":["UMEMBER"]}`))
				default:
					t.Errorf("unexpected API %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}, user)
			id, err := a.Send(t.Context(), Conversation("slack", "CTEST", "123.000001"), domain.OutboundMessage{Text: "**bold** [link](https://example.test)", Attachments: []domain.Attachment{{Ref: "FTEST"}}})
			require.NoError(t, err)
			require.Equal(t, "124.000001", id)
			groups, err := a.UserGroups(t.Context(), []string{"GTEST"})
			require.NoError(t, err)
			require.Equal(t, []string{"UMEMBER"}, groups["GTEST"])
		})
	}
}

func TestSocketMapping(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"USELF","bot_id":"BSELF"}`))
		case "/users.info":
			_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"UMEMBER","is_bot":false}}`))
		case "/conversations.replies":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
		default:
			t.Errorf("unexpected API %s", r.URL.Path)
		}
	}, false)
	stream := make(chan socketmode.Event)
	acks := make(chan string, 10)
	a.socketEvents = stream
	a.runSocket = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	a.ack = func(_ context.Context, id string, _ any) error { acks <- id; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan domain.InboundEvent, 10)
	done := make(chan error, 1)
	go func() {
		done <- a.Start(ctx, func(_ context.Context, e domain.InboundEvent) error { events <- e; return nil })
	}()
	select {
	case stream <- socketmode.Event{Type: socketmode.EventTypeHello, Request: &socketmode.Request{Type: socketmode.RequestTypeHello}}:
	case <-time.After(time.Second):
		t.Fatal("hello blocked")
	}
	for i, raw := range []string{
		`{"type":"message","channel":"CTEST","user":"USELF","text":"self","ts":"1"}`,
		`{"type":"message","channel":"CTEST","bot_id":"BSELF","text":"self","ts":"2","subtype":"bot_message"}`,
		`{"type":"message","channel":"CTEST","user":"UMEMBER","text":"hi <@USELF>","ts":"124.000001","thread_ts":"123.000001","files":[{"id":"FTEST","name":"a.png","mimetype":"image/png"}]}`,
		`{"type":"message","channel":"DTEST","subtype":"message_changed","message":{"user":"UMEMBER","text":"edited","ts":"125.000001"}}`,
		`{"type":"reaction_added","user":"UMEMBER","item_user":"USELF","reaction":"white_check_mark","item":{"type":"message","channel":"CTEST","ts":"124.000001"}}`,
		`{"type":"message","subtype":"bot_message","channel":"CTEST","bot_id":"BOTHER","text":"alert","ts":"126.000001"}`,
		`{"type":"message","channel":"CTEST","user":"UMEMBER","text":"unjoined","ts":"127.000001","thread_ts":"126.000001"}`,
		`{"type":"reaction_added","user":"UMEMBER","item_user":"UOTHER","reaction":"eyes","item":{"type":"message","channel":"CTEST","ts":"126.000001"}}`,
	} {
		payload := json.RawMessage(`{"type":"event_callback","event":` + raw + `}`)
		parsed, err := slackevents.ParseEvent(payload, slackevents.OptionNoVerifyToken())
		require.NoError(t, err)
		select {
		case stream <- socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: parsed, Request: &socketmode.Request{EnvelopeID: "envelope"}}:
		case <-time.After(time.Second):
			t.Fatal("stream blocked", i)
		}
		require.Equal(t, "envelope", <-acks)
	}
	for _, kind := range []domain.InboundEventKind{domain.InboundEventMessage, domain.InboundEventMessageEdited, domain.InboundEventReaction, domain.InboundEventMessage} {
		select {
		case e := <-events:
			require.Equal(t, kind, e.Kind)
			if e.Text == "hi <@USELF>" {
				require.True(t, e.MentionsMe)
				require.Equal(t, Conversation("slack", "CTEST", "123.000001"), e.Conversation)
				require.Equal(t, Conversation("slack", "CTEST", ""), e.Parent)
				require.Equal(t, "FTEST", e.Attachments[0].Ref)
			}
			if e.Kind == domain.InboundEventMessageEdited {
				require.Equal(t, Conversation("slack", "DTEST", ""), e.Conversation)
				require.Equal(t, "edited", e.Text)
			}
			if e.Kind == domain.InboundEventReaction {
				require.Equal(t, domain.SenderHuman, e.Sender.Kind)
				require.Equal(t, Conversation("slack", "CTEST", ""), e.Conversation)
				require.Contains(t, e.Text, "white_check_mark")
			}
			if e.Text == "alert" {
				require.Equal(t, domain.SenderBot, e.Sender.Kind)
				require.Equal(t, "BOTHER", e.Sender.ID)
			}
		case <-time.After(time.Second):
			t.Fatal("missing event")
		}
	}
	cancel()
	require.NoError(t, <-done)
	select {
	case e := <-events:
		t.Fatalf("unexpected event %+v", e)
	default:
	}
}

func TestMarkdown(t *testing.T) {
	require.Equal(t, "*Title*\n• *bold* <https://example.test|site>\n1. item\n`**literal**`\n```\n# raw\n```\n<@USER> &amp; &lt;plain&gt;", mrkdwn("# Title\n- **bold** [site](https://example.test)\n1. item\n`**literal**`\n```\n# raw\n```\n<@USER> & <plain>"))
}

func TestMarkdownAroundLiterals(t *testing.T) {
	require.Equal(t, "*bold*", mrkdwn("__bold__"))
	require.Equal(t, "*run `code` now*\n*Use `tool`*", mrkdwn("**run `code` now**\n# Use `tool`"))
	require.Equal(t, "*Hi <@USER>*", mrkdwn("# Hi <@USER>"))
}

func TestBotReactionClassification(t *testing.T) {
	for _, ok := range []bool{false, true} {
		a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/users.info" && ok {
				_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"UBOT","is_bot":true}}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
		}, false)
		a.selfUser = "USELF"
		data, err := slackevents.ParseEvent(json.RawMessage(`{"type":"event_callback","event":{"type":"reaction_added","user":"UBOT","item_user":"USELF","reaction":"eyes","item":{"type":"message","channel":"CTEST","ts":"123.000001"}}}`), slackevents.OptionNoVerifyToken())
		require.NoError(t, err)
		event, accepted := a.inbound(t.Context(), data.InnerEvent.Data)
		require.True(t, accepted)
		require.Equal(t, domain.SenderBot, event.Sender.Kind)
	}
}

func TestMessageSourceReferences(t *testing.T) {
	a := &adapter{name: "slack", acceptConversation: func(_ domain.ConversationRef) bool { return false }}
	for _, channel := range []string{"DTEST", "CTEST", "GTEST"} {
		for _, thread := range []string{"", "1.2"} {
			event, ok := a.inbound(t.Context(), &slackevents.MessageEvent{Channel: channel, User: "U", TimeStamp: "2.3", ThreadTimeStamp: thread})
			require.True(t, ok)
			require.Equal(t, channel == "DTEST", event.Direct)
			require.Equal(t, Conversation("slack", channel, "2.3"), event.MessageRef)
			require.Equal(t, Conversation("slack", channel, thread), event.Conversation)
		}
	}
}

func TestDeletedMessageKeepsSenderAndThread(t *testing.T) {
	a := &adapter{name: "slack", selfUser: "UBOT", acceptConversation: func(_ domain.ConversationRef) bool { return false }}
	parsed, err := slackevents.ParseEvent(json.RawMessage(`{"type":"event_callback","event":{"type":"message","subtype":"message_deleted","channel":"CTEST","previous_message":{"user":"UMEMBER","text":"old body","ts":"2.000001","thread_ts":"1.000001"}}}`), slackevents.OptionNoVerifyToken())
	require.NoError(t, err)
	event, ok := a.inbound(t.Context(), parsed.InnerEvent.Data)
	require.True(t, ok)
	require.True(t, event.Deleted)
	require.Equal(t, domain.InboundEventMessageEdited, event.Kind)
	require.Equal(t, "UMEMBER", event.Sender.ID)
	require.Equal(t, Conversation("slack", "CTEST", "1.000001"), event.Conversation)
	require.Empty(t, event.Text)
}

func TestOwnerDirectMessageUsesBot(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "/conversations.open", r.URL.Path)
		require.Equal(t, "xoxb-test", r.Form.Get("token"))
		require.Equal(t, "UOWNER", r.Form.Get("users"))
		_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"DOWNER"}}`))
	}, true)
	ref, err := a.DirectMessage(t.Context(), "UOWNER")
	require.NoError(t, err)
	require.Equal(t, Conversation("slack", "DOWNER", ""), ref)
}

func TestUnfurlEditsAreDropped(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		accepted            bool
	}{
		{"empty files", `"text":"link"`, `"text":"link","files":[]`, false},
		{"unfurl", `"text":"link","files":[]`, `"text":"link","files":[],"attachments":[{"title":"preview"}]`, false},
		{"text", `"text":"old"`, `"text":"new"`, true},
		{"file", `"text":"same","files":[{"id":"F1"}]`, `"text":"same","files":[{"id":"F2"}]`, true},
		{"file metadata", `"text":"same","files":[{"id":"F1","name":"old"}]`, `"text":"same","files":[{"id":"F1","name":"new","size":120}]`, false},
		{"same file", `"text":"same","files":[{"id":"F1"}]`, `"text":"same","files":[{"id":"F1"}],"attachments":[{"title":"preview"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"type":"event_callback","event":{"type":"message","subtype":"message_changed","channel":"CTEST","previous_message":{"user":"UMEMBER",` + tc.before + `},"message":{"user":"UMEMBER",` + tc.after + `}}}`
			parsed, err := slackevents.ParseEvent(json.RawMessage(raw), slackevents.OptionNoVerifyToken())
			require.NoError(t, err)
			_, accepted := (&adapter{name: "slack"}).inbound(t.Context(), parsed.InnerEvent.Data)
			require.Equal(t, tc.accepted, accepted)
		})
	}
}

func TestDeniedReactionDoesNotReadRemoteUserOrMessage(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected lookup: %s", r.URL.Path) }, false)
	a.selfUser = "USELF"
	a.SetConversationFilter(func(ref domain.ConversationRef) bool { return ref == Conversation("slack", "CALLOWED", "") })
	data, err := slackevents.ParseEvent(json.RawMessage(`{"type":"event_callback","event":{"type":"reaction_added","user":"UMEMBER","item_user":"USELF","reaction":"eyes","item":{"type":"message","channel":"CDENIED","ts":"123.000001"}}}`), slackevents.OptionNoVerifyToken())
	require.NoError(t, err)
	event, present := a.inbound(t.Context(), data.InnerEvent.Data)
	require.True(t, present)
	require.Equal(t, domain.InboundEventReaction, event.Kind)
	require.Equal(t, Conversation("slack", "CDENIED", ""), event.Conversation)
	require.Empty(t, event.Parent)
	require.Equal(t, "UMEMBER", event.Sender.ID)
	require.Contains(t, event.Text, "eyes")
}
