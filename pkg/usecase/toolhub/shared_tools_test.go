package toolhub_test

import (
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/usecase/reconcile"
	"github.com/motoki317/noctiluca/pkg/usecase/toolhub"
	"github.com/stretchr/testify/require"
)

func TestSharedScheduleTools(t *testing.T) {
	h := startHub(t, toolhub.Config{})
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "test.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	schedules := repository.NewScheduleRepository(db)
	r := reconcile.New(reconcile.Config{}, nil, schedules, nil, nil, nil, nil)
	require.NoError(t, h.AddSource("schedules", r.Tools()))
	endpoint, err := h.OpenSession("room")
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

	builtIns := []domain.Schedule{
		{Name: domain.HeartbeatSchedule, Cron: "@every 1h", TZ: "UTC"},
		{Name: domain.RestSchedule, Cron: "0 0 * * *", TZ: "UTC"},
	}
	require.NoError(t, r.Install(t.Context(), builtIns))
	installed, err := schedules.List(t.Context())
	require.NoError(t, err)
	require.Len(t, installed, len(builtIns))
	for _, job := range builtIns {
		t.Run(job.Name, func(t *testing.T) {
			add := call(t, c, "schedule_add", map[string]any{"name": job.Name, "cron": "0 9 * * *", "tz": "UTC", "prompt": "work"})
			require.True(t, add.IsError)
			remove := call(t, c, "schedule_remove", map[string]any{"name": job.Name})
			require.True(t, remove.IsError)
			require.Equal(t, add.Content, remove.Content)
		})
	}
	t.Run("empty prompt", func(t *testing.T) {
		result := call(t, c, "schedule_add", map[string]any{"name": "blank", "cron": "0 9 * * *", "tz": "UTC", "prompt": " \t"})
		require.True(t, result.IsError)
		require.Equal(t, "empty prompt", result.Content[0].(*mcp.TextContent).Text)
	})
	jobs, err = schedules.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, installed, jobs)
}
