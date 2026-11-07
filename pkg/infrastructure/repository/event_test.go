package repository_test

import (
	"context"
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
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, r.SaveInbound(ctx, event), context.Canceled)
	_, err = r.Search(ctx, "first", 10)
	require.ErrorIs(t, err, context.Canceled)
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
		{"bcd", 0}, {"ABC", 0}, {"ab", 0}, {"f", 0}, {"東京駅", 1}, {"写真", 1}, {"京", 1},
		{"%", 2}, {"_", 2}, {`\`, 2}, {`"quoted"`, 2}, {"OR NOT", 2}, {"100%", 2},
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
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = r.GetInbound(ctx, event.MessageRef)
	require.ErrorIs(t, err, context.Canceled)
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
