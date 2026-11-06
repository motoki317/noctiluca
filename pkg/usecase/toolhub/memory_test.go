package toolhub_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/usecase/consolidate"
	memoryusecase "github.com/motoki317/noctiluca/pkg/usecase/memory"
	"github.com/motoki317/noctiluca/pkg/usecase/reconcile"
	"github.com/stretchr/testify/require"
)

func TestEverySessionCanManageMemoryAndSchedules(t *testing.T) {
	h := startHub(t)
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "owner"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "owner/soul.md"), []byte("handwritten"), 0600))
	store, err := memory.New(t.Context(), memory.Config{Path: dir})
	require.NoError(t, err)
	history := store.(domain.MemoryHistory)
	baseline, err := history.Revision(t.Context())
	require.NoError(t, err)
	require.NoError(t, h.AddSource("memory", memoryusecase.Tools(store)))
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
			for _, args := range []map[string]any{
				{"path": "owner/soul.md", "content": "changed", "message": "Attempt write"},
				{"path": "owner/soul.md", "delete": true, "message": "Attempt delete"},
			} {
				result := call(t, c, "memory_write", args)
				require.True(t, result.IsError)
				require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "owner/ is read-only")
			}
			require.True(t, call(t, c, "memory_revert", map[string]any{"commit": baseline}).IsError)
			for _, path := range []string{"core.md", "procedures/check.md", "research/note.md"} {
				result := call(t, c, "memory_write", map[string]any{"path": path, "content": "model memory", "message": "Record memory"})
				require.False(t, result.IsError)
				var written struct{ Revision string }
				require.NoError(t, json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &written))
				require.False(t, call(t, c, "memory_revert", map[string]any{"commit": written.Revision}).IsError)
				_, err := store.Read(t.Context(), path)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
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
