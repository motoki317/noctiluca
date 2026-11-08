package repository_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/usecase/consolidate"
	"github.com/stretchr/testify/require"
)

func TestEpisodesWindowAndCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := repository.New(t.Context(), repository.Config{Path: path})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	events := repository.NewEventRepository(db)
	repo := repository.NewEpisodeRepository(db)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	last, err := repo.LastRun(t.Context())
	require.NoError(t, err)
	require.True(t, last.IsZero())
	require.NoError(t, events.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "room", SentAt: at.Add(-time.Second), Message: domain.OutboundMessage{Text: "Before cutoff"}}))
	accepted := true
	for i, text := range []string{"That's wrong", "I didn't say that"} {
		require.NoError(t, events.SaveInbound(t.Context(), domain.InboundEvent{Kind: domain.InboundEventMessage, Accepted: &accepted, Conversation: "room", Sender: domain.Sender{ID: "owner", Kind: domain.SenderHuman}, Text: text, ReceivedAt: at.Add(time.Duration(i) * time.Second)}))
	}
	require.NoError(t, events.SaveModel(t.Context(), domain.ModelRecord{SessionKey: "room", RecordedAt: at.Add(2 * time.Second), Event: domain.ModelEvent{Kind: domain.ModelEventToolCall, ToolCall: &domain.ToolCall{Name: "read", Input: json.RawMessage(`{}`)}}}))
	require.NoError(t, events.SaveModel(t.Context(), domain.ModelRecord{SessionKey: "room", RecordedAt: at.Add(3 * time.Second), Event: domain.ModelEvent{Kind: domain.ModelEventToolResult, Err: errors.New("read failed\nfile not found")}}))
	require.NoError(t, events.SaveInbound(t.Context(), domain.InboundEvent{Kind: domain.InboundEventMessageEdited, Deleted: true, Conversation: "room", Sender: domain.Sender{ID: "owner", Kind: domain.SenderHuman}, Text: "[message deleted]", ReceivedAt: at.Add(4 * time.Second)}))
	require.NoError(t, events.SaveModel(t.Context(), domain.ModelRecord{SessionKey: "task:consolidate", RecordedAt: at.Add(4500 * time.Millisecond), Event: domain.ModelEvent{Kind: domain.ModelEventText, Text: "Ordinary task history"}}))
	entries, err := repo.Since(t.Context(), at, at.Add(5*time.Second))
	require.NoError(t, err)
	require.Len(t, entries, 5)
	for _, entry := range entries {
		require.NotEqual(t, domain.SessionKey("task:consolidate"), entry.SessionKey)
	}
	require.True(t, entries[0].AfterSend)
	require.NotNil(t, entries[0].Accepted)
	require.True(t, *entries[0].Accepted)
	require.False(t, entries[1].AfterSend)
	require.Equal(t, "read failed\nfile not found", entries[3].Error)
	for _, i := range []int{0, 1, 2, 4} {
		require.Empty(t, entries[i].Error)
	}
	require.True(t, entries[4].Deleted)
	require.NoError(t, repo.SaveLastRun(t.Context(), at))
	require.NoError(t, repo.SaveLastRun(t.Context(), at.Add(-time.Hour)))
	require.NoError(t, db.Close())
	db, err = repository.New(t.Context(), repository.Config{Path: path})
	require.NoError(t, err)
	repo = repository.NewEpisodeRepository(db)
	last, err = repo.LastRun(t.Context())
	require.NoError(t, err)
	require.Equal(t, at, last)
	batch, err := consolidate.EpisodesSince(t.Context(), repo, time.Time{}, at.Add(5*time.Second), nil)
	require.NoError(t, err)
	require.Equal(t, at, batch.Since)
	require.Len(t, batch.Items, 2)
	require.Contains(t, batch.Items[0].Signals, "user_correction")
	require.Contains(t, batch.Items[1].Signals, "tool_error")
	require.Equal(t, []string{"read failed … file not found"}, batch.Items[1].Errors)
	require.Contains(t, batch.Items[1].Signals, "user_deleted")
	bounded, err := repo.Since(t.Context(), at, at.Add(time.Second))
	require.NoError(t, err)
	require.Len(t, bounded, 1)
}

func TestEpisodeOutcomeAcrossCheckpoint(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "history.db")})
	require.NoError(t, err)
	defer db.Close()
	events := repository.NewEventRepository(db)
	repo := repository.NewEpisodeRepository(db)
	at := time.Now().UTC()
	yes, no := true, false
	require.NoError(t, events.SaveInbound(t.Context(), domain.InboundEvent{Conversation: "room", Kind: domain.InboundEventMessage, Sender: domain.Sender{Kind: domain.SenderHuman}, Text: "Please read the file", Accepted: &yes, ReceivedAt: at.Add(-time.Second)}))
	require.NoError(t, events.SaveInbound(t.Context(), domain.InboundEvent{Conversation: "room", Kind: domain.InboundEventMessage, Sender: domain.Sender{Kind: domain.SenderHuman}, Text: "ignored", Accepted: &no, ReceivedAt: at}))
	require.NoError(t, events.SaveModel(t.Context(), domain.ModelRecord{SessionKey: "room", RecordedAt: at.Add(time.Second), Event: domain.ModelEvent{Kind: domain.ModelEventToolResult, Err: errors.New("read failed")}}))
	batch, err := consolidate.EpisodesSince(t.Context(), repo, at, at.Add(2*time.Second), nil)
	require.NoError(t, err)
	require.Len(t, batch.Items, 2)
	require.Equal(t, "Please read the file", batch.Items[0].Input)
	require.Contains(t, batch.Items[0].Signals, "tool_error")
	require.False(t, batch.Items[0].Ambiguous)

}

func TestQueuedInputAcrossCheckpointRemainsAmbiguous(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "history.db")})
	require.NoError(t, err)
	defer db.Close()
	events := repository.NewEventRepository(db)
	repo := repository.NewEpisodeRepository(db)
	at := time.Now().UTC()
	accepted := true
	for i, text := range []string{"First input still running", "Queued second input"} {
		require.NoError(t, events.SaveInbound(t.Context(), domain.InboundEvent{Conversation: "room", Kind: domain.InboundEventMessage, Sender: domain.Sender{Kind: domain.SenderHuman}, Text: text, Accepted: &accepted, ReceivedAt: at.Add(time.Duration(2*i-1) * time.Second)}))
	}
	require.NoError(t, events.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "room", Message: domain.OutboundMessage{Text: "Outcome of first input"}, SentAt: at.Add(2 * time.Second)}))
	batch, err := consolidate.EpisodesSince(t.Context(), repo, at, at.Add(3*time.Second), nil)
	require.NoError(t, err)
	require.Len(t, batch.Items, 2)
	for _, item := range batch.Items {
		require.True(t, item.Ambiguous)
		require.Empty(t, item.Reply)
	}
}

func TestQueuedTurnStartsAcrossCheckpoint(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "history.db")})
	require.NoError(t, err)
	defer db.Close()
	events := repository.NewEventRepository(db)
	repo := repository.NewEpisodeRepository(db)
	at := time.Now().UTC()
	accepted := true
	for i, text := range []string{"A", "B", "C"} {
		offset := []int{-4, -3, 1}[i]
		require.NoError(t, events.SaveInbound(t.Context(), domain.InboundEvent{Conversation: "room", Kind: domain.InboundEventMessage, Sender: domain.Sender{Kind: domain.SenderHuman}, Text: text, Accepted: &accepted, ReceivedAt: at.Add(time.Duration(offset) * time.Second)}))
	}
	for i, kind := range []domain.ModelEventKind{domain.ModelEventTurnFinished, domain.ModelEventTurnStarted} {
		require.NoError(t, events.SaveModel(t.Context(), domain.ModelRecord{SessionKey: "room", Event: domain.ModelEvent{Kind: kind}, RecordedAt: at.Add(time.Duration(i-2) * time.Second)}))
	}
	require.NoError(t, events.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "room", Message: domain.OutboundMessage{Text: "B's reply"}, SentAt: at.Add(2 * time.Second)}))
	batch, err := consolidate.EpisodesSince(t.Context(), repo, at, at.Add(3*time.Second), nil)
	require.NoError(t, err)
	require.Len(t, batch.Items, 2)
	require.Equal(t, "B", batch.Items[0].Input)
	for _, item := range batch.Items {
		require.True(t, item.Ambiguous)
		require.Empty(t, item.Reply)
	}
}
