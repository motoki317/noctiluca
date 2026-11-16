package repository_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/usecase/consolidate"
	"github.com/stretchr/testify/require"
)

func TestEpisodesWindowAndCheckpoint(t *testing.T) {
	db := openDB(t)
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
	last, err = repo.LastRun(t.Context())
	require.NoError(t, err)
	require.Equal(t, at, last)
	batch, err := consolidate.EpisodesSince(t.Context(), repo, time.Time{}, at.Add(5*time.Second), nil)
	require.NoError(t, err)
	require.Equal(t, at, batch.Since)
	bounded, err := repo.Since(t.Context(), at, at.Add(time.Second))
	require.NoError(t, err)
	require.Len(t, bounded, 1)
}

func TestEpisodeOutcomeAcrossCheckpoint(t *testing.T) {
	db := openDB(t)
	events := repository.NewEventRepository(db)
	repo := repository.NewEpisodeRepository(db)
	at := time.Now().UTC()
	yes, no := true, false
	require.NoError(t, events.SaveInbound(t.Context(), domain.InboundEvent{Conversation: "room", Kind: domain.InboundEventMessage, Sender: domain.Sender{Kind: domain.SenderHuman}, Text: "Please read the file", Accepted: &yes, ReceivedAt: at.Add(-time.Second)}))
	require.NoError(t, events.SaveInbound(t.Context(), domain.InboundEvent{Conversation: "room", Kind: domain.InboundEventMessage, Sender: domain.Sender{Kind: domain.SenderHuman}, Text: "ignored", Accepted: &no, ReceivedAt: at}))
	require.NoError(t, events.SaveModel(t.Context(), domain.ModelRecord{SessionKey: "room", RecordedAt: at.Add(time.Second), Event: domain.ModelEvent{Kind: domain.ModelEventToolResult, Err: errors.New("read failed")}}))
	require.NoError(t, events.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "room", SentAt: at.Add(time.Second), Message: domain.OutboundMessage{Text: "Could not read the file"}}))
	batch, err := consolidate.EpisodesSince(t.Context(), repo, at, at.Add(2*time.Second), nil)
	require.NoError(t, err)
	require.Len(t, batch.Items, 3)
	require.Equal(t, "Please read the file", batch.Items[0].Input)
	require.Equal(t, "Could not read the file", batch.Items[0].Reply)
	require.Empty(t, batch.Items[0].Signals)
	require.False(t, batch.Items[0].Ambiguous)
	require.Contains(t, batch.Items[2].Signals, "tool_error")

}

func TestQueuedInputAcrossCheckpointRemainsAmbiguous(t *testing.T) {
	db := openDB(t)
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
	db := openDB(t)
	events := repository.NewEventRepository(db)
	repo := repository.NewEpisodeRepository(db)
	at := time.Now().UTC()
	accepted := true
	for i, text := range []string{"A", "B", "C"} {
		offset := []int{-4, -3, 1}[i]
		receiveEpisodeInput(t, db, "room", domain.InboundEvent{Conversation: "room", Kind: domain.InboundEventMessage, Sender: domain.Sender{Kind: domain.SenderHuman}, Text: text, Accepted: &accepted, ReceivedAt: at.Add(time.Duration(offset) * time.Second)})
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

func receiveEpisodeInput(t *testing.T, db *sql.DB, key domain.SessionKey, event domain.InboundEvent) {
	t.Helper()
	item := domain.InboxItem{SourceKey: fmt.Sprintf("%s:%d", event.Conversation, event.ReceivedAt.UnixNano()), SessionKey: key, Lane: "conv", ReadyAt: event.ReceivedAt, Payload: domain.InboxPayload{Event: &event}}
	require.NoError(t, repository.NewInboxRepository(db).Receive(t.Context(), event, item))
}

func TestEpisodesSeparateConversationsAndModelTurns(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%t", shared), func(t *testing.T) {
			db := openDB(t)
			events := repository.NewEventRepository(db)
			repo := repository.NewEpisodeRepository(db)
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			accepted := true
			key := func(conversation domain.ConversationRef) domain.SessionKey {
				if shared {
					return "shared"
				}
				return domain.SessionKey(conversation)
			}
			input := func(conversation domain.ConversationRef, offset int, text string) {
				receiveEpisodeInput(t, db, key(conversation), domain.InboundEvent{Conversation: conversation, MessageRef: domain.ConversationRef(text), Kind: domain.InboundEventMessage, Sender: domain.Sender{Kind: domain.SenderHuman}, Text: text, Accepted: &accepted, ReceivedAt: at.Add(time.Duration(offset) * time.Second)})
			}
			model := func(conversation domain.ConversationRef, offset int, event domain.ModelEvent) {
				require.NoError(t, events.SaveModel(t.Context(), domain.ModelRecord{SessionKey: key(conversation), RecordedAt: at.Add(time.Duration(offset) * time.Second), Event: event}))
			}
			send := func(conversation domain.ConversationRef, offset int, text string) {
				require.NoError(t, events.SaveOutbound(t.Context(), domain.SentMessage{Conversation: conversation, SentAt: at.Add(time.Duration(offset) * time.Second), Message: domain.OutboundMessage{Text: text}}))
			}
			input("room-a", 0, "first-a")
			model("room-a", 1, domain.ModelEvent{Kind: domain.ModelEventTurnStarted})
			input("room-b", 2, "first-b")
			if !shared {
				model("room-b", 3, domain.ModelEvent{Kind: domain.ModelEventTurnStarted})
			}
			model("room-a", 4, domain.ModelEvent{Kind: domain.ModelEventToolCall, ToolCall: &domain.ToolCall{Name: "read", Input: json.RawMessage(`{}`)}})
			model("room-a", 5, domain.ModelEvent{Kind: domain.ModelEventToolResult, Err: errors.New("read failed")})
			send("room-a", 6, "answer-a")
			input("room-a", 7, "second-a")
			send("room-a", 8, "second-answer-a")
			send("room-b", 9, "answer-b")
			model("room-a", 10, domain.ModelEvent{Kind: domain.ModelEventTurnFinished})
			if !shared {
				model("room-b", 10, domain.ModelEvent{Kind: domain.ModelEventTurnFinished})
			}
			input("room-b", 11, "second-b")
			model("room-b", 12, domain.ModelEvent{Kind: domain.ModelEventTurnStarted})
			model("room-b", 13, domain.ModelEvent{Kind: domain.ModelEventToolCall, ToolCall: &domain.ToolCall{Name: "read", Input: json.RawMessage(`{}`)}})
			model("room-b", 14, domain.ModelEvent{Kind: domain.ModelEventTurnFinished})
			input("room-b", 15, "third-b")
			send("room-b", 16, "third-answer-b")
			batch, err := consolidate.EpisodesSince(t.Context(), repo, at, at.Add(17*time.Second), nil)
			require.NoError(t, err)
			require.Len(t, batch.Items, 7)
			byInput := map[string]consolidate.Episode{}
			var turns []consolidate.Episode
			for _, episode := range batch.Items {
				require.False(t, episode.Ambiguous)
				if episode.Conversation == "" {
					turns = append(turns, episode)
					continue
				}
				require.Equal(t, key(episode.Conversation), episode.SessionKey)
				require.Empty(t, episode.Tools)
				require.Empty(t, episode.Errors)
				require.Empty(t, episode.Signals)
				byInput[episode.Input] = episode
			}
			require.Equal(t, "answer-a", byInput["first-a"].Reply)
			require.Equal(t, "second-answer-a", byInput["second-a"].Reply)
			require.Equal(t, "answer-b", byInput["first-b"].Reply)
			require.Empty(t, byInput["second-b"].Reply)
			require.Equal(t, "third-answer-b", byInput["third-b"].Reply)
			require.Len(t, turns, 2)
			require.Equal(t, []string{"read"}, turns[0].Tools)
			require.Equal(t, []string{"read failed"}, turns[0].Errors)
			require.Equal(t, []string{"tool_error"}, turns[0].Signals)
			require.Equal(t, []string{"read"}, turns[1].Tools)
			require.Empty(t, turns[1].Signals)
		})
	}
}

func TestEpisodeCheckpointUsesHandlingSession(t *testing.T) {
	for _, replied := range []bool{false, true} {
		t.Run(fmt.Sprintf("replied=%t", replied), func(t *testing.T) {
			db := openDB(t)
			events := repository.NewEventRepository(db)
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			accepted := true
			for i, offset := range []int{-4, -3, 1} {
				receiveEpisodeInput(t, db, "shared", domain.InboundEvent{Conversation: "room", Kind: domain.InboundEventMessage, Sender: domain.Sender{Kind: domain.SenderHuman}, Text: fmt.Sprintf("input-%d", i), Accepted: &accepted, ReceivedAt: at.Add(time.Duration(offset) * time.Second)})
			}
			for i, kind := range []domain.ModelEventKind{domain.ModelEventTurnFinished, domain.ModelEventTurnStarted} {
				require.NoError(t, events.SaveModel(t.Context(), domain.ModelRecord{SessionKey: "shared", Event: domain.ModelEvent{Kind: kind}, RecordedAt: at.Add(time.Duration(i-2) * time.Second)}))
			}
			if replied {
				require.NoError(t, events.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "room", SentAt: at.Add(-time.Second), Message: domain.OutboundMessage{Text: "prior answer"}}))
			}
			require.NoError(t, events.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "room", SentAt: at.Add(2 * time.Second), Message: domain.OutboundMessage{Text: "new answer"}}))
			batch, err := consolidate.EpisodesSince(t.Context(), repository.NewEpisodeRepository(db), at, at.Add(3*time.Second), nil)
			require.NoError(t, err)
			require.Len(t, batch.Items, 2)
			require.True(t, batch.Items[0].Ambiguous)
			require.Empty(t, batch.Items[0].Reply)
			require.Equal(t, !replied, batch.Items[1].Ambiguous)
			if replied {
				require.Equal(t, "new answer", batch.Items[1].Reply)
			} else {
				require.Empty(t, batch.Items[1].Reply)
			}
		})
	}
}

func TestEpisodeCheckpointKeepsCompletedConversationClosed(t *testing.T) {
	db := openDB(t)
	events := repository.NewEventRepository(db)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accepted := true
	for _, input := range []struct {
		conversation domain.ConversationRef
		offset       int
		text         string
	}{
		{"room-a", -5, "completed-a"},
		{"room-b", -2, "running-b"},
		{"room-a", 1, "new-a"},
	} {
		receiveEpisodeInput(t, db, "shared", domain.InboundEvent{Conversation: input.conversation, Kind: domain.InboundEventMessage, Sender: domain.Sender{Kind: domain.SenderHuman}, Text: input.text, Accepted: &accepted, ReceivedAt: at.Add(time.Duration(input.offset) * time.Second)})
	}
	for i, kind := range []domain.ModelEventKind{domain.ModelEventTurnStarted, domain.ModelEventTurnFinished, domain.ModelEventTurnStarted} {
		require.NoError(t, events.SaveModel(t.Context(), domain.ModelRecord{SessionKey: "shared", Event: domain.ModelEvent{Kind: kind}, RecordedAt: at.Add(time.Duration(i-4) * time.Second)}))
	}
	require.NoError(t, events.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "room-a", SentAt: at.Add(2 * time.Second), Message: domain.OutboundMessage{Text: "new answer"}}))
	batch, err := consolidate.EpisodesSince(t.Context(), repository.NewEpisodeRepository(db), at, at.Add(3*time.Second), nil)
	require.NoError(t, err)
	require.Len(t, batch.Items, 2)
	require.Equal(t, "completed-a", batch.Items[0].Input)
	require.Empty(t, batch.Items[0].Reply)
	require.Equal(t, "new-a", batch.Items[1].Input)
	require.Equal(t, "new answer", batch.Items[1].Reply)
	for _, episode := range batch.Items {
		require.False(t, episode.Ambiguous)
	}
}
