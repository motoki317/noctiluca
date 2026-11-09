package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/require"
)

func TestInstanceReferencesAndTools(t *testing.T) {
	for _, name := range []string{"slack", "support_2"} {
		t.Run(name, func(t *testing.T) {
			a := apiInstance(t, name, func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				switch r.URL.Path {
				case "/conversations.open":
					fmt.Fprint(w, `{"ok":true,"channel":{"id":"D1"}}`)
				case "/chat.postMessage":
					require.Equal(t, "C1", r.Form.Get("channel"))
					require.Equal(t, "1.2", r.Form.Get("thread_ts"))
					fmt.Fprint(w, `{"ok":true,"ts":"2.3"}`)
				case "/conversations.info":
					fmt.Fprint(w, `{"ok":true,"channel":{"name":"general"}}`)
				case "/conversations.replies":
					fmt.Fprint(w, `{"ok":true,"messages":[{"user":"USELF","ts":"1.2","text":"root"}]}`)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}, false)
			ref := domain.ConversationRef(name + ":C1:1.2")
			require.Equal(t, ref, Conversation(name, "C1", "1.2"))
			id, err := a.Send(t.Context(), ref, domain.OutboundMessage{Text: "hello"})
			require.NoError(t, err)
			require.Equal(t, "2.3", id)
			_, err = a.Send(t.Context(), "other:C1:1.2", domain.OutboundMessage{})
			require.ErrorContains(t, err, "invalid Slack conversation reference")
			dm, err := a.DirectMessage(t.Context(), "U1")
			require.NoError(t, err)
			require.Equal(t, domain.ConversationRef(name+":D1:"), dm)
			info := a.SourceInfo(t.Context(), ref, "", true)
			require.Equal(t, "#general", info.Conversation)
			require.Equal(t, "root", info.ThreadText)
			a.selfUser = "USELF"
			var filtered domain.ConversationRef
			a.SetConversationFilter(func(ref domain.ConversationRef, _ bool) bool { filtered = ref; return true })
			event, ok := a.inbound(t.Context(), &slackevents.MessageEvent{Channel: "C1", User: "U1", TimeStamp: "2.3", ThreadTimeStamp: "1.2"})
			require.True(t, ok)
			require.Equal(t, ref, event.Conversation)
			require.Equal(t, domain.ConversationRef(name+":C1:"), event.Parent)
			require.Equal(t, event.Parent, filtered)
			require.Equal(t, domain.ConversationRef(name+":C1:2.3"), event.MessageRef)
			_, joined := a.joinedThreads.Load(ref)
			require.True(t, joined)
			a.inbound(t.Context(), &slackevents.MessageEvent{Channel: "C1", User: "USELF", TimeStamp: "3.4"})
			_, joined = a.joinedThreads.Load(domain.ConversationRef(name + ":C1:3.4"))
			require.True(t, joined)
			a.SetConversationFilter(func(domain.ConversationRef, bool) bool { return false })
			reaction, ok := a.inbound(t.Context(), &slackevents.ReactionAddedEvent{User: "U1", ItemUser: "USELF", Item: slackevents.Item{Type: "message", Channel: "C1", Timestamp: "3.4"}})
			require.True(t, ok)
			require.Equal(t, domain.ConversationRef(name+":C1:"), reaction.Conversation)
			require.Equal(t, domain.ConversationRef(name+":C1:3.4"), reaction.MessageRef)
			tools := a.Tools()
			suffixes := []string{"read_thread", "read_history", "search", "download_file", "upload_file", "react", "set_status", "send"}
			require.Len(t, tools, len(suffixes))
			for i, suffix := range suffixes {
				require.Equal(t, name+"_"+suffix, tools[i].Name)
				require.Equal(t, i < 3, tools[i].ReadOnly)
			}
			legacyNotes := "A ref ending in a colon is top level: do not append a timestamp. " +
				"Use slack_read_thread, slack_read_history, and slack_search for context. Slack tools take bare channel IDs. " +
				"To broadcast a PNG, JPEG, or GIF image, use slack_upload_file with thread_ts and broadcast=true. " +
				"When you decide to post in a thread, call slack_set_status on it before your other tools. The work target of a top-level channel message is its thread. " +
				"Your post there clears the status, so call it again when work continues after a post. Skip it when the target you post to ends in a colon."
			require.Equal(t, strings.ReplaceAll(legacyNotes, "slack_", name+"_"), a.ToolNotes())
		})
	}
}

func TestInstanceSocketSourceKeys(t *testing.T) {
	for _, name := range []string{"slack", "support_2"} {
		t.Run(name, func(t *testing.T) {
			a := apiInstance(t, name, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/auth.test", r.URL.Path)
				fmt.Fprint(w, `{"ok":true,"user_id":"USELF","bot_id":"BSELF"}`)
			}, false)
			stream := make(chan socketmode.Event)
			a.socketEvents = stream
			a.runSocket = func(ctx context.Context) error { <-ctx.Done(); return nil }
			a.ack = func(context.Context, string, any) error { return nil }
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			events := make(chan domain.InboundEvent, 1)
			done := make(chan error, 1)
			go func() {
				done <- a.Start(ctx, func(_ context.Context, event domain.InboundEvent) error { events <- event; return nil })
			}()
			defer func() { cancel(); require.NoError(t, <-done) }()
			for _, tc := range []struct{ eventID, envelope, want string }{{"event-id", "envelope-id", "event-id"}, {"", "envelope-id", "envelope-id"}} {
				payload, err := slackevents.ParseEvent(json.RawMessage(fmt.Sprintf(`{"type":"event_callback","event_id":%q,"event":{"type":"message","channel":"D1","user":"U1","ts":"1.2"}}`, tc.eventID)), slackevents.OptionNoVerifyToken())
				require.NoError(t, err)
				select {
				case stream <- socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: payload, Request: &socketmode.Request{EnvelopeID: tc.envelope}}:
				case <-ctx.Done():
					t.Fatal("socket did not start")
				}
				select {
				case event := <-events:
					require.Equal(t, name+":"+tc.want, event.SourceKey)
					require.Equal(t, domain.ConversationRef(name+":D1:"), event.Conversation)
					require.Equal(t, domain.ConversationRef(name+":D1:1.2"), event.MessageRef)
				case <-ctx.Done():
					t.Fatal("event was not delivered")
				}
			}
		})
	}
}
