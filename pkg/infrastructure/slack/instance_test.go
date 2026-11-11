package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
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
			a.SetConversationFilter(func(ref domain.ConversationRef) bool { filtered = ref; return true })
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
			a.SetConversationFilter(func(domain.ConversationRef) bool { return false })
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
			require.Equal(t, "Upload a local file. channel and thread_ts come from the ref you post to; omit thread_ts for a ref ending in a colon. Set broadcast with thread_ts to show a PNG, JPEG, or GIF image in the channel too.", tools[4].Description)
			require.Equal(t, "Post Markdown text. channel and thread_ts come from the ref you post to; omit thread_ts for a ref ending in a colon. Set broadcast on a thread reply to also show it in the channel.", tools[7].Description)
			notes := "A ref ending in a colon is top level: do not append a timestamp. " +
				"A top-level channel input (a message or a reaction) also has a `thread` ref, the thread under that message. Post chat and quick answers at `conversation`, and work (commands, investigations, reviews, drafts, and their background results) at `thread`. " +
				"Other inputs have only `conversation`; post there. " +
				"Use slack_read_thread, slack_read_history, and slack_search for context. Slack tools take bare channel IDs. " +
				"To broadcast a PNG, JPEG, or GIF image, use slack_upload_file with thread_ts and broadcast=true. " +
				"If an answer needs any tool besides the reply, first show that you started where you will answer: for a thread, call slack_set_status on it, and again when work continues after you post there; for a ref ending in a colon, react to the input `message` with slack_react."
			require.Equal(t, strings.ReplaceAll(notes, "slack_", name+"_"), a.ToolNotes())
		})
	}
}

func TestInstanceSocketSourceKeys(t *testing.T) {
	for _, name := range []string{"slack", "support_2"} {
		t.Run(name, func(t *testing.T) {
			a := apiInstance(t, name, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/auth.test":
					fmt.Fprint(w, `{"ok":true,"user_id":"USELF","bot_id":"BSELF"}`)
				case "/users.info":
					fmt.Fprint(w, `{"ok":true,"user":{"id":"U1","is_bot":false}}`)
				default:
					t.Errorf("unexpected API %s", r.URL.Path)
				}
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
			for _, tc := range []struct{ event, eventID, envelope, want string }{
				{`{"type":"message","channel":"D1","user":"U1","ts":"1.2"}`, "event-id", "envelope-id", "m:D1:1.2"},
				{`{"type":"message","channel":"D1","user":"U1","ts":"1.2"}`, "", "envelope-id", "m:D1:1.2"},
				{`{"type":"message","subtype":"message_changed","channel":"D1","message":{"user":"U1","ts":"1.2","text":"edited"}}`, "edit-id", "envelope-id", "edit-id"},
				{`{"type":"message","subtype":"message_changed","channel":"D1","message":{"user":"U1","ts":"1.2","text":"edited"}}`, "", "envelope-id", "envelope-id"},
				{`{"type":"message","subtype":"message_deleted","channel":"D1","previous_message":{"user":"U1","ts":"1.2"}}`, "delete-id", "envelope-id", "delete-id"},
				{`{"type":"message","subtype":"message_deleted","channel":"D1","previous_message":{"user":"U1","ts":"1.2"}}`, "", "envelope-id", "envelope-id"},
				{`{"type":"reaction_added","user":"U1","reaction":"eyes","item":{"type":"message","channel":"D1","ts":"1.2"}}`, "reaction-id", "envelope-id", "reaction-id"},
				{`{"type":"reaction_added","user":"U1","reaction":"eyes","item":{"type":"message","channel":"D1","ts":"1.2"}}`, "", "envelope-id", "envelope-id"},
			} {
				payload, err := slackevents.ParseEvent(json.RawMessage(fmt.Sprintf(`{"type":"event_callback","event_id":%q,"event":%s}`, tc.eventID, tc.event)), slackevents.OptionNoVerifyToken())
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

func TestSocketInboxDeduplication(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "inbox.db")})
	require.NoError(t, err)
	defer db.Close()
	inbox := repository.NewInboxRepository(db)
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth.test":
			fmt.Fprint(w, `{"ok":true,"user_id":"USELF","bot_id":"BSELF"}`)
		case "/users.info":
			fmt.Fprint(w, `{"ok":true,"user":{"id":"U1","is_bot":false}}`)
		default:
			t.Errorf("unexpected API %s", r.URL.Path)
		}
	}, false)
	stream := make(chan socketmode.Event)
	acks := make(chan string, 1)
	a.socketEvents = stream
	a.runSocket = func(ctx context.Context) error { <-ctx.Done(); return nil }
	a.ack = func(_ context.Context, id string, _ any) error { acks <- id; return nil }
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- a.Start(ctx, func(ctx context.Context, event domain.InboundEvent) error {
			return inbox.Receive(ctx, event, domain.InboxItem{
				SourceKey: event.SourceKey, SessionKey: domain.SessionKey(event.Conversation), Lane: "conv",
				ReadyAt: event.ReceivedAt, Payload: domain.InboxPayload{Event: &event},
			})
		})
	}()
	defer func() { cancel(); require.NoError(t, <-done) }()
	for _, tc := range []struct {
		event, id string
		want      int
	}{
		{`{"type":"message","channel":"D1","user":"U1","ts":"1.2"}`, "message-a", 1},
		{`{"type":"message","channel":"D1","user":"U1","ts":"1.2"}`, "message-b", 1},
		{`{"type":"reaction_added","user":"U1","reaction":"eyes","item":{"type":"message","channel":"D1","ts":"1.2"}}`, "reaction-a", 2},
		{`{"type":"reaction_added","user":"U1","reaction":"eyes","item":{"type":"message","channel":"D1","ts":"1.2"}}`, "reaction-b", 3},
	} {
		payload, err := slackevents.ParseEvent(json.RawMessage(fmt.Sprintf(`{"type":"event_callback","event_id":%q,"event":%s}`, tc.id, tc.event)), slackevents.OptionNoVerifyToken())
		require.NoError(t, err)
		select {
		case stream <- socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: payload, Request: &socketmode.Request{EnvelopeID: tc.id}}:
		case <-ctx.Done():
			t.Fatal("socket did not accept event")
		}
		select {
		case id := <-acks:
			require.Equal(t, tc.id, id)
		case <-ctx.Done():
			t.Fatal("event was not acknowledged")
		}
		items, err := inbox.Active(t.Context())
		require.NoError(t, err)
		require.Len(t, items, tc.want)
	}
}
