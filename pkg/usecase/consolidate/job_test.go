package consolidate

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func TestCheckpoint(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	episodes := repository.NewEpisodeRepository(db)
	checkpoint := Checkpoint(episodes)
	at := time.Now().UTC()
	require.NoError(t, checkpoint(t.Context(), "consolidate", at))
	for _, procedure := range []string{"", "heartbeat"} {
		require.NoError(t, checkpoint(t.Context(), procedure, at.Add(time.Hour)))
	}
	saved, err := episodes.LastRun(t.Context())
	require.NoError(t, err)
	require.Equal(t, at, saved)
}

func TestEpisodesTool(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, repository.NewEventRepository(db).SaveInbound(t.Context(), domain.InboundEvent{Sender: domain.Sender{Kind: domain.SenderHuman}, Conversation: "room", Kind: domain.InboundEventMessage, Text: "The release is Friday", ReceivedAt: time.Now().Add(-time.Minute)}))
	raw, err := EpisodesTool(repository.NewEpisodeRepository(db), nil).Handler(t.Context(), json.RawMessage(`{}`))
	require.NoError(t, err)
	var batch Episodes
	require.NoError(t, json.Unmarshal(raw, &batch))
	require.Len(t, batch.Items, 1)
	require.Equal(t, "The release is Friday", batch.Items[0].Input)
}
