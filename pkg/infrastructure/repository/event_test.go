package repository_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func TestEventHistory(t *testing.T) {
	r := repository.NewEventRepository(openDB(t))
	at := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	event := domain.InboundEvent{Kind: domain.InboundEventMessage, Conversation: "thread", Parent: "room", MessageRef: "message", Direct: true,
		Sender: domain.Sender{ID: "user", DisplayName: "Member", Kind: domain.SenderHuman}, Text: "first",
		Attachments: []domain.Attachment{{Ref: "file", Name: "image.png", MIMEType: "image/png"}}, MentionsMe: true, ReceivedAt: at}
	require.NoError(t, r.SaveInbound(t.Context(), event))
	for _, kind := range []domain.InboundEventKind{domain.InboundEventMessageEdited, domain.InboundEventReaction} {
		next := event
		next.Kind, next.Text, next.Sender.Kind = kind, string(kind), domain.SenderBot
		next.ReceivedAt = at.Add(time.Nanosecond).In(time.FixedZone("offset", 9*3600))
		require.NoError(t, r.SaveInbound(t.Context(), next))
	}
	other := event
	other.Conversation = "other"
	require.NoError(t, r.SaveInbound(t.Context(), other))
	got, err := r.ListInbound(t.Context(), "thread", 3)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, domain.InboundEventReaction, got[0].Kind)
	assert.True(t, at.Add(time.Nanosecond).Equal(got[0].ReceivedAt))
	assert.Equal(t, event, got[2])
	got, err = r.ListInbound(t.Context(), "thread", 1)
	require.NoError(t, err)
	assert.Len(t, got, 1)

	sent := domain.SentMessage{ID: "message", Conversation: "thread", Message: domain.OutboundMessage{Text: "reply", Attachments: event.Attachments}, SentAt: at}
	require.NoError(t, r.SaveOutbound(t.Context(), sent))
	messages, err := r.ListOutbound(t.Context(), "thread", 10)
	require.NoError(t, err)
	assert.Equal(t, []domain.SentMessage{sent}, messages)
	messages, err = r.ListOutbound(t.Context(), "other", 10)
	require.NoError(t, err)
	assert.Empty(t, messages)
	_, err = r.ListInbound(t.Context(), "thread", 0)
	require.Error(t, err)
	_, err = r.ListOutbound(t.Context(), "thread", -1)
	require.Error(t, err)
}

func TestListConversations(t *testing.T) {
	r := repository.NewEventRepository(openDB(t))
	refs, err := r.ListConversations(t.Context(), "work_chat")
	require.NoError(t, err)
	require.Empty(t, refs)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	accepted, rejected := true, false
	for _, event := range []domain.InboundEvent{
		{Kind: domain.InboundEventMessage, Conversation: "work_chat:room:", Accepted: &rejected, Direct: true},
		{Kind: domain.InboundEventMessage, Conversation: "work_chat:room:"},
		{Kind: domain.InboundEventMessage, Conversation: "work_chat:room:", Accepted: &accepted},
		{Kind: domain.InboundEventMessage, Conversation: "work_chat:room:", Accepted: &accepted},
		{Kind: domain.InboundEventMessage, Conversation: "work_chat:unknown:"},
		{Kind: domain.InboundEventMessageEdited, Conversation: "work_chat:edited:", Accepted: &accepted},
		{Kind: domain.InboundEventMessageEdited, Conversation: "work_chat:deleted:", Deleted: true, Accepted: &accepted},
		{Kind: domain.InboundEventMessage, Conversation: "work_chat:dm:", Direct: true, Accepted: &accepted},
		{Kind: domain.InboundEventReaction, Conversation: "work_chat:reaction:", Accepted: &accepted},
		{Kind: domain.InboundEventMessage, Conversation: "work_chat:thread_room:thread", Parent: "work_chat:thread_room:", Accepted: &accepted},
		{Kind: domain.InboundEventMessage, Conversation: "work_chat_extra:room:", Accepted: &accepted},
		{Kind: domain.InboundEventMessage, Conversation: "workxchat:room:", Accepted: &accepted},
		{Kind: domain.InboundEventMessage, Conversation: "other:room:", Accepted: &accepted},
		{Kind: domain.InboundEventMessage, Conversation: "work_chat:room:thread:", Accepted: &accepted},
		{Kind: domain.InboundEventMessage, Conversation: "work_chat::", Accepted: &accepted},
	} {
		event.Sender = domain.Sender{ID: "member", Kind: domain.SenderHuman}
		event.ReceivedAt = at
		require.NoError(t, r.SaveInbound(t.Context(), event))
	}
	require.NoError(t, r.SaveOutbound(t.Context(), domain.SentMessage{
		ID: "reply", Conversation: "work_chat:outbound:", Message: domain.OutboundMessage{Text: "reply"}, SentAt: at,
	}))
	refs, err = r.ListConversations(t.Context(), "work_chat")
	require.NoError(t, err)
	require.Equal(t, []domain.Conversation{{Ref: "work_chat:deleted:"}, {Ref: "work_chat:dm:", Direct: true}, {Ref: "work_chat:edited:"}, {Ref: "work_chat:room:"}}, refs)
	refs, err = r.ListConversations(t.Context(), "missing")
	require.NoError(t, err)
	require.Empty(t, refs)
}

func TestListThreads(t *testing.T) {
	r := repository.NewEventRepository(openDB(t))
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("offset", 9*3600))
	refs, err := r.ListThreads(t.Context(), "work_chat", since)
	require.NoError(t, err)
	require.Empty(t, refs)
	accepted := true
	require.NoError(t, r.SaveInbound(t.Context(), domain.InboundEvent{Kind: domain.InboundEventMessage,
		Conversation: "work_chat:room:unlisted", Accepted: &accepted, Sender: domain.Sender{Kind: domain.SenderHuman}, ReceivedAt: since}))
	refs, err = r.ListThreads(t.Context(), "work_chat", since)
	require.NoError(t, err)
	require.Empty(t, refs)
	for _, sent := range []domain.SentMessage{
		{Conversation: "work_chat:room:sent", SentAt: since.Add(time.Hour)},
		{Conversation: "work_chat:room:sent", SentAt: since.Add(2 * time.Hour)},
		{Conversation: "work_chat:room:message", SentAt: since},
		{Conversation: "work_chat:room:old_send", SentAt: since.Add(-time.Nanosecond)},
		{Conversation: "work_chat:room:", SentAt: since},
		{Conversation: "work_chat:room:thread:extra", SentAt: since},
		{Conversation: "work_chat::thread", SentAt: since},
		{Conversation: "workxchat:room:thread", SentAt: since},
		{Conversation: "work_chat_extra:room:thread", SentAt: since},
		{Conversation: "other:room:thread", SentAt: since},
	} {
		require.NoError(t, r.SaveOutbound(t.Context(), sent))
	}
	refs, err = r.ListThreads(t.Context(), "work_chat", since)
	require.NoError(t, err)
	require.Equal(t, []domain.ConversationRef{"work_chat:room:message", "work_chat:room:sent"}, refs)
	refs, err = r.ListThreads(t.Context(), "missing", since)
	require.NoError(t, err)
	require.Empty(t, refs)
}

func TestLatestMessageTime(t *testing.T) {
	r := repository.NewEventRepository(openDB(t))
	got, err := r.LatestMessageTime(t.Context(), "room")
	require.NoError(t, err)
	require.True(t, got.IsZero())
	at := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	for _, tc := range []struct {
		name    string
		kind    domain.InboundEventKind
		deleted bool
		room    domain.ConversationRef
		offset  time.Duration
		want    time.Duration
	}{
		{"message", domain.InboundEventMessage, false, "room", 0, 0},
		{"edit", domain.InboundEventMessageEdited, false, "room", time.Second, time.Second},
		{"late older message", domain.InboundEventMessage, false, "room", 0, time.Second},
		{"reaction", domain.InboundEventReaction, false, "room", time.Hour, time.Second},
		{"deletion", domain.InboundEventMessageEdited, true, "room", 2 * time.Hour, time.Second},
		{"other conversation", domain.InboundEventMessage, false, "other", 3 * time.Hour, time.Second},
	} {
		require.NoError(t, r.SaveInbound(t.Context(), domain.InboundEvent{Kind: tc.kind, Deleted: tc.deleted,
			Conversation: tc.room, Sender: domain.Sender{Kind: domain.SenderHuman}, ReceivedAt: at.Add(tc.offset)}), tc.name)
		got, err := r.LatestMessageTime(t.Context(), "room")
		require.NoError(t, err, tc.name)
		require.Equal(t, at.Add(tc.want), got, tc.name)
	}
}

func TestEventSearch(t *testing.T) {
	r := repository.NewEventRepository(openDB(t))
	texts := []string{"abcdef", "東京駅で写真を見る", `100% _ \\ "quoted" OR NOT`, "unrelated"}
	for i, text := range texts {
		require.NoError(t, r.SaveInbound(t.Context(), domain.InboundEvent{Kind: domain.InboundEventMessage,
			Conversation: domain.ConversationRef(fmt.Sprint(i)), Sender: domain.Sender{Kind: domain.SenderHuman}, Text: text,
			ReceivedAt: time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC)}))
	}
	for _, tt := range []struct {
		query string
		index int
	}{
		{"bcd", 0}, {"ABC", 0}, {"ab", 0}, {"東京駅", 1}, {"写真", 1},
		{"%", 2}, {"_", 2}, {`\`, 2}, {`"quoted"`, 2}, {"OR NOT", 2},
		{"", -1}, {"missing", -1}, {`x' OR 1=1 --`, -1},
	} {
		t.Run(tt.query, func(t *testing.T) {
			got, err := r.Search(t.Context(), tt.query, 10)
			require.NoError(t, err)
			if tt.index < 0 {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			assert.Equal(t, texts[tt.index], got[0].Text)
		})
	}
	for _, query := range []string{"\xff", "nul\x00"} {
		_, err := r.Search(t.Context(), query, 10)
		require.Error(t, err)
	}
	_, err := r.Search(t.Context(), "abc", -1)
	require.Error(t, err)
}

func TestGetInboundByMessageRef(t *testing.T) {
	r := repository.NewEventRepository(openDB(t))
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	accepted := false
	event := domain.InboundEvent{Kind: domain.InboundEventMessage, Conversation: "chat:room:thread", Parent: "chat:room:",
		MessageRef: "chat:room:message", Sender: domain.Sender{ID: "author", DisplayName: "Author", Kind: domain.SenderHuman},
		Text: "original", ReceivedAt: at, Direct: true, MentionsMe: true, Accepted: &accepted,
		Attachments: []domain.Attachment{{Ref: "file", Name: "photo.png", MIMEType: "image/png"}}}
	require.NoError(t, r.SaveInbound(t.Context(), event))
	got, err := r.GetInbound(t.Context(), event.MessageRef)
	require.NoError(t, err)
	require.Equal(t, event, got)

	event.Kind, event.Text, event.ReceivedAt = domain.InboundEventMessageEdited, "edited", at.Add(time.Second)
	require.NoError(t, r.SaveInbound(t.Context(), event))
	event.Text = "last edit at the same timestamp"
	require.NoError(t, r.SaveInbound(t.Context(), event))
	older := event
	older.ReceivedAt, older.Text = at, "older edit received late"
	require.NoError(t, r.SaveInbound(t.Context(), older))
	reaction := event
	reaction.Kind, reaction.Sender.ID, reaction.ReceivedAt = domain.InboundEventReaction, "reactor", at.Add(time.Hour)
	require.NoError(t, r.SaveInbound(t.Context(), reaction))
	other := reaction
	other.Kind, other.MessageRef = domain.InboundEventMessage, "other:room:message"
	require.NoError(t, r.SaveInbound(t.Context(), other))
	got, err = r.GetInbound(t.Context(), event.MessageRef)
	require.NoError(t, err)
	require.Equal(t, event, got)

	reaction.MessageRef = "chat:room:reaction_only"
	require.NoError(t, r.SaveInbound(t.Context(), reaction))
	other.MessageRef = ""
	require.NoError(t, r.SaveInbound(t.Context(), other))
	for _, ref := range []domain.ConversationRef{"", "missing", reaction.MessageRef, "chat:room:message' OR 1=1 --"} {
		_, err := r.GetInbound(t.Context(), ref)
		require.ErrorIs(t, err, domain.ErrNotFound, ref)
	}
}

func TestConcurrentWrites(t *testing.T) {
	r := repository.NewEventRepository(openDB(t))
	var wg sync.WaitGroup
	errors := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			errors <- r.SaveInbound(t.Context(), domain.InboundEvent{Kind: domain.InboundEventMessage,
				Conversation: "room", Sender: domain.Sender{Kind: domain.SenderHuman}, Text: "concurrent", ReceivedAt: time.Now()})
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	got, err := r.Search(t.Context(), "concurrent", 20)
	require.NoError(t, err)
	assert.Len(t, got, 12)
	got, err = r.Search(t.Context(), "concurrent", 2)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}
