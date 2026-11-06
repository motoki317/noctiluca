package repository_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func TestInputContextUsesPostingOrderAndDeliveredHistory(t *testing.T) {
	db := openDB(t)
	r := repository.NewEventRepository(db)
	inbox := repository.NewInboxRepository(db)
	at := time.Date(2026, 11, 6, 7, 29, 1, 0, time.UTC)
	root := domain.InboundEvent{Kind: domain.InboundEventMessage, Conversation: "room", MessageRef: "thread", Sender: domain.Sender{ID: "person", Kind: domain.SenderHuman}, Text: "Thread subject", ReceivedAt: at.Add(-time.Hour)}
	require.NoError(t, r.SaveInbound(t.Context(), root))
	for _, event := range []domain.ModelRecord{
		{SessionKey: "room", Event: domain.ModelEvent{Kind: domain.ModelEventTurnStarted}, RecordedAt: at.Add(-time.Second)},
		{SessionKey: "room", Event: domain.ModelEvent{Kind: domain.ModelEventTurnFinished}, RecordedAt: at.Add(2 * time.Second)},
	} {
		require.NoError(t, r.SaveModel(t.Context(), event))
	}
	require.NoError(t, r.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "thread", SentAt: at.Add(time.Second)}))
	require.NoError(t, r.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "other", SentAt: at.Add(time.Hour)}))
	info, err := r.InputContext(t.Context(), "room", "thread", at)
	require.NoError(t, err)
	require.True(t, info.Running, "a later finish must not hide the running turn at posting time")
	require.Equal(t, at.Add(time.Second), info.LastReply)
	require.Equal(t, "Thread subject", info.Root.Text)
	require.False(t, info.ThreadSeen)
	for _, accepted := range []bool{false, true} {
		event := root
		event.Conversation, event.Parent, event.MessageRef = "thread", "room", "reply"
		event.Accepted = &accepted
		item := domain.InboxItem{SourceKey: "rejected", SessionKey: "room", Lane: "conv", Status: "done", Payload: domain.InboxPayload{Event: &event}}
		if accepted {
			item.SourceKey = "delivered"
		}
		_, err := inbox.Enqueue(t.Context(), item)
		require.NoError(t, err)
		info, err := r.InputContext(t.Context(), "room", "thread", at.Add(3*time.Second))
		require.NoError(t, err)
		require.Equal(t, accepted, info.ThreadSeen)
		require.False(t, info.Running)
	}
}

func TestInputContextCountsDeliveredContinuations(t *testing.T) {
	db := openDB(t)
	r := repository.NewEventRepository(db)
	inbox := repository.NewInboxRepository(db)
	_, err := inbox.Enqueue(t.Context(), domain.InboxItem{SourceKey: "watch:1", SessionKey: "room", Lane: "conv", Status: "done", Payload: domain.InboxPayload{Target: "thread"}})
	require.NoError(t, err)
	info, err := r.InputContext(t.Context(), "room", "thread", time.Now())
	require.NoError(t, err)
	require.True(t, info.ThreadSeen)
	other, err := r.InputContext(t.Context(), "other", "thread", time.Now())
	require.NoError(t, err)
	require.False(t, other.ThreadSeen)
}

func TestInputContextUsesCurrentRootText(t *testing.T) {
	r := repository.NewEventRepository(openDB(t))
	root := domain.InboundEvent{Kind: domain.InboundEventMessage, Conversation: "room", MessageRef: "thread", Sender: domain.Sender{ID: "person", Kind: domain.SenderHuman}, Text: "Original", ReceivedAt: time.Now()}
	require.NoError(t, r.SaveInbound(t.Context(), root))
	for _, deleted := range []bool{false, true} {
		root.Kind, root.Deleted, root.Text = domain.InboundEventMessageEdited, deleted, "Edited"
		root.ReceivedAt = root.ReceivedAt.Add(time.Second)
		if deleted {
			root.Text = "[message deleted]"
		}
		require.NoError(t, r.SaveInbound(t.Context(), root))
		reaction := root
		reaction.Kind, reaction.Text, reaction.ReceivedAt = domain.InboundEventReaction, "reaction", root.ReceivedAt.Add(time.Second)
		require.NoError(t, r.SaveInbound(t.Context(), reaction))
		info, err := r.InputContext(t.Context(), "room", "thread", time.Now())
		require.NoError(t, err)
		require.Equal(t, root.Text, info.Root.Text)
	}
}
