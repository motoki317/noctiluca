package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	sdk "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

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
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "events.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a, err := newAdapter(name, config, repository.NewEventRepository(db), sdk.OptionAPIURL(server.URL+"/"), sdk.OptionHTTPClient(server.Client()))
	require.NoError(t, err)
	return a
}

func TestSendAndGroups(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		switch r.URL.Path {
		case "/files.info":
			require.Equal(t, "xoxp-test", r.Form.Get("token"))
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
	}, true)
	id, err := a.Send(t.Context(), Conversation("slack", "CTEST", "123.000001"), domain.OutboundMessage{Text: "**bold** [link](https://example.test)", Attachments: []domain.Attachment{{Ref: "FTEST"}}})
	require.NoError(t, err)
	require.Equal(t, "124.000001", id)
	groups, err := a.UserGroups(t.Context(), []string{"GTEST"})
	require.NoError(t, err)
	require.Equal(t, []string{"UMEMBER"}, groups["GTEST"])
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

func TestBotMessageClassification(t *testing.T) {
	event, accepted := (&adapter{name: "slack"}).inbound(t.Context(), &slackevents.MessageEvent{
		SubType: "bot_message", Channel: "CTEST", BotID: "BOTHER", Text: "alert", TimeStamp: "126.000001",
	})
	require.True(t, accepted)
	require.Equal(t, domain.SenderBot, event.Sender.Kind)
	require.Equal(t, "BOTHER", event.Sender.ID)
}

func TestMessageSourceReferences(t *testing.T) {
	a := &adapter{name: "slack", acceptConversation: func(_ domain.ConversationRef) bool { return false }}
	for _, tc := range []struct{ channel, thread string }{{"DTEST", ""}, {"CTEST", "1.2"}} {
		event, ok := a.inbound(t.Context(), &slackevents.MessageEvent{Channel: tc.channel, User: "U", TimeStamp: "2.3", ThreadTimeStamp: tc.thread})
		require.True(t, ok)
		require.Equal(t, tc.channel == "DTEST", event.Direct)
		require.Equal(t, Conversation("slack", tc.channel, "2.3"), event.MessageRef)
		require.Equal(t, Conversation("slack", tc.channel, tc.thread), event.Conversation)
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

func TestUnfurlEditsAreDropped(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		accepted            bool
	}{
		{"unfurl", `"text":"link","files":[]`, `"text":"link","files":[],"attachments":[{"title":"preview"}]`, false},
		{"text", `"text":"old"`, `"text":"new"`, true},
		{"file", `"text":"same","files":[{"id":"F1"}]`, `"text":"same","files":[{"id":"F2"}]`, true},
		{"file metadata", `"text":"same","files":[{"id":"F1","name":"old"}]`, `"text":"same","files":[{"id":"F1","name":"new","size":120}]`, false},
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
