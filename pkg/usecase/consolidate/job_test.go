package consolidate

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func TestCheckpoint(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	checkpoint := Checkpoint(repository.NewConsolidationRepository(db))
	at := time.Now().UTC()
	require.NoError(t, checkpoint(t.Context(), "consolidate", at))
	for _, procedure := range []string{"", "heartbeat"} {
		require.NoError(t, checkpoint(t.Context(), procedure, at.Add(time.Hour)))
	}
	var saved time.Time
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT last_run FROM consolidation WHERE id=1`).Scan(&saved))
	require.Equal(t, at, saved)
}
