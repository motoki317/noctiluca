package toolhub_test

import (
	"path/filepath"
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/usecase/consolidate"
	"github.com/motoki317/noctiluca/pkg/usecase/reconcile"
	"github.com/motoki317/noctiluca/pkg/usecase/toolhub"
	"github.com/stretchr/testify/require"
)

func TestEverySessionCanManageSchedulesAndReadEpisodes(t *testing.T) {
	h := startHub(t, toolhub.Config{})
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "test.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	schedules := repository.NewScheduleRepository(db)
	r := reconcile.New(reconcile.Config{}, nil, schedules, nil, nil, nil, nil)
	require.NoError(t, h.AddSource("schedules", r.Tools()))
	require.NoError(t, h.AddSource("episodes", []domain.Tool{consolidate.EpisodesTool(repository.NewEpisodeRepository(db), nil)}))
	for _, key := range []domain.SessionKey{"room", "dm", "task:consolidate"} {
		t.Run(string(key), func(t *testing.T) {
			endpoint, err := h.OpenSession(key)
			require.NoError(t, err)
			c := client(t, endpoint)
			require.False(t, call(t, c, "schedule_add", map[string]any{"name": "daily", "cron": "0 9 * * *", "tz": "UTC", "prompt": "Check tasks"}).IsError)
			jobs, err := schedules.List(t.Context())
			require.NoError(t, err)
			require.Len(t, jobs, 1)
			require.False(t, call(t, c, "schedule_remove", map[string]any{"name": "daily"}).IsError)
			jobs, err = schedules.List(t.Context())
			require.NoError(t, err)
			require.Empty(t, jobs)
			require.False(t, call(t, c, "episodes_since", map[string]any{}).IsError)
		})
	}
}
