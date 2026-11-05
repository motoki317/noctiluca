package repository_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func TestModelHistory(t *testing.T) {
	r := repository.NewEventRepository(openDB(t))
	at := time.Now().UTC()
	events := []domain.ModelEvent{
		{Kind: domain.ModelEventTurnStarted},
		{Kind: domain.ModelEventToolCall, ToolCall: &domain.ToolCall{ID: "call", Name: "reply", Input: json.RawMessage(`{"text":"hello"}`)}},
		{Kind: domain.ModelEventBackgroundDone, Text: "done"},
		{Kind: domain.ModelEventError, Err: errors.New("failed")},
	}
	for _, event := range events {
		require.NoError(t, r.SaveModel(t.Context(), domain.ModelRecord{SessionKey: "room", Event: event, RecordedAt: at}))
	}
	records, err := r.ListModel(t.Context(), "room", 3)
	require.NoError(t, err)
	require.Len(t, records, 3)
	require.EqualError(t, records[0].Event.Err, "failed")
	require.Equal(t, events[2], records[1].Event)
	require.Equal(t, events[1], records[2].Event)
	require.Equal(t, at, records[2].RecordedAt)
	records, err = r.ListModel(t.Context(), "other", 3)
	require.NoError(t, err)
	require.Empty(t, records)
	_, err = r.ListModel(t.Context(), "room", 0)
	require.Error(t, err)
}
