package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/require"
)

func TestInstanceReferencesAndTools(t *testing.T) {
	const name = "support_2"
	a := apiInstance(t, name, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		switch r.URL.Path {
		case "/conversations.open":
			fmt.Fprint(w, `{"ok":true,"channel":{"id":"D1"}}`)
		case "/chat.postMessage":
			require.Equal(t, "C1", r.Form.Get("channel"))
			require.Equal(t, "1.2", r.Form.Get("thread_ts"))
			fmt.Fprint(w, `{"ok":true,"ts":"2.3"}`)
		case "/conversations.replies":
			fmt.Fprint(w, `{"ok":true,"messages":[{"user":"USELF","ts":"1.2","text":"root"}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}, false)
	ref := domain.ConversationRef(name + ":C1:1.2")
	_, err := a.Send(t.Context(), ref, domain.OutboundMessage{Text: "hello"})
	require.NoError(t, err)
	_, err = a.Send(t.Context(), "other:C1:1.2", domain.OutboundMessage{})
	require.ErrorContains(t, err, "invalid Slack conversation reference")
	dm, err := a.DirectMessage(t.Context(), "U1")
	require.NoError(t, err)
	require.Equal(t, domain.ConversationRef(name+":D1:"), dm)
	a.selfUser = "USELF"
	var filtered domain.ConversationRef
	a.SetConversationFilter(func(ref domain.ConversationRef) bool { filtered = ref; return true })
	event, ok := a.inbound(t.Context(), &slackevents.MessageEvent{Channel: "C1", User: "U1", TimeStamp: "2.3", ThreadTimeStamp: "1.2"})
	require.True(t, ok)
	require.Equal(t, ref, event.Conversation)
	require.Equal(t, domain.ConversationRef(name+":C1:"), event.Parent)
	require.Equal(t, event.Parent, filtered)
	require.Equal(t, domain.ConversationRef(name+":C1:2.3"), event.MessageRef)
	require.Equal(t, name+":m:C1:2.3", event.SourceKey)
	a.SetConversationFilter(func(domain.ConversationRef) bool { return false })
	reaction, ok := a.inbound(t.Context(), &slackevents.ReactionAddedEvent{User: "U1", ItemUser: "USELF", Reaction: "eyes", Item: slackevents.Item{Type: "message", Channel: "C1", Timestamp: "3.4"}})
	require.True(t, ok)
	require.Equal(t, domain.InboundEventReaction, reaction.Kind)
	require.Equal(t, domain.ConversationRef(name+":C1:"), reaction.Conversation)
	require.Equal(t, domain.ConversationRef(name+":C1:3.4"), reaction.MessageRef)
	require.Empty(t, reaction.Parent)
	require.Equal(t, "U1", reaction.Sender.ID)
	require.Contains(t, reaction.Text, "eyes")
	tools := a.Tools()
	for i, suffix := range []string{"read_thread", "read_history", "search", "download_file", "upload_file", "react", "set_status", "send"} {
		require.Equal(t, name+"_"+suffix, tools[i].Name)
		require.Equal(t, i < 3, tools[i].ReadOnly)
	}
	notes := a.ToolNotes()
	require.Contains(t, notes, "Post chat and quick answers at `conversation`, and work (commands, investigations, reviews, drafts, and their background results) at `thread`.")
	require.Contains(t, notes, "for a thread, call support_2_set_status on it, and again when work continues after you post there")
	require.Contains(t, notes, "for a ref ending in a colon, react to the input `message` with support_2_react")
}

func TestInstanceSocketSourceKeys(t *testing.T) {
	const name = "support_2"
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
	acks := make(chan string, 1)
	a.socketEvents = stream
	a.runSocket = func(ctx context.Context) error { <-ctx.Done(); return nil }
	a.ack = func(_ context.Context, id string, _ any) error { acks <- id; return nil }
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	events := make(chan domain.InboundEvent, 1)
	done := make(chan error, 1)
	go func() {
		done <- a.Start(ctx, func(_ context.Context, event domain.InboundEvent) error { events <- event; return nil })
	}()
	defer func() { cancel(); require.NoError(t, <-done) }()
	for _, tc := range []struct{ event, eventID, want string }{
		{`{"type":"message","channel":"D1","user":"U1","ts":"1.2"}`, "event-id", "m:D1:1.2"},
		{`{"type":"reaction_added","user":"U1","reaction":"eyes","item":{"type":"message","channel":"D1","ts":"1.2"}}`, "reaction-id", "reaction-id"},
		{`{"type":"reaction_added","user":"U1","reaction":"eyes","item":{"type":"message","channel":"D1","ts":"1.2"}}`, "", "envelope-id"},
		{`{"type":"message","channel":"D1","user":"USELF","ts":"1.2"}`, "dropped-id", ""},
	} {
		payload, err := slackevents.ParseEvent(json.RawMessage(fmt.Sprintf(`{"type":"event_callback","event_id":%q,"event":%s}`, tc.eventID, tc.event)), slackevents.OptionNoVerifyToken())
		require.NoError(t, err)
		select {
		case stream <- socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: payload, Request: &socketmode.Request{EnvelopeID: "envelope-id"}}:
		case <-ctx.Done():
			t.Fatal("socket did not start")
		}
		select {
		case id := <-acks:
			require.Equal(t, "envelope-id", id)
		case <-ctx.Done():
			t.Fatal("event was not acknowledged")
		}
		if tc.want == "" {
			require.Empty(t, events)
			continue
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
}
