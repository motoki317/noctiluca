package watch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func take[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(time.Second):
		t.Fatal("timeout")
		var zero T
		return zero
	}
}
func setup(t *testing.T) (domain.WatchRepository, domain.InboxRepository, func() domain.Background) {
	t.Helper()
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "history.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	watches, inbox := repository.NewWatchRepository(db), repository.NewInboxRepository(db)
	return watches, inbox, func() domain.Background {
		return New(watches, inbox, func(_ context.Context, key domain.SessionKey) (domain.InboxItem, error) {
			return domain.InboxItem{SessionKey: key, Lane: "conv", Payload: domain.InboxPayload{Context: domain.SessionContext{Conversation: "origin"}}}, nil
		}, func() {})
	}
}
func nextNote(t *testing.T, inbox domain.InboxRepository, count int) string {
	t.Helper()
	var items []domain.InboxItem
	require.Eventually(t, func() bool {
		var err error
		items, err = inbox.Active(t.Context())
		return err == nil && len(items) == count
	}, time.Second, time.Millisecond)
	return items[count-1].Payload.Input.Text
}
func start(t *testing.T, w domain.Background) (context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()
	impl := w.(*watcher)
	require.Eventually(t, func() bool { impl.mu.Lock(); defer impl.mu.Unlock(); return impl.ctx != nil }, time.Second, time.Millisecond)
	return cancel, done
}
func toolContext(t *testing.T) context.Context {
	return domain.WithToolSession(t.Context(), domain.ToolSession{Key: "origin"})
}

func TestBackgroundToolRefDescriptions(t *testing.T) {
	w := &watcher{}
	for i, description := range []string{
		"Schedule a note at an RFC3339 time, then end this turn. The note returns with the ref you pass as target.",
		"Start a shell command and end this turn immediately. Its exit code and output tail return as a new input with the ref you pass as target.",
	} {
		tool := w.Tools()[i]
		require.Equal(t, description, tool.Description)
		var schema struct {
			Properties map[string]struct{ Description string }
		}
		require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
		require.Equal(t, "The ref where the result should be posted.", schema.Properties["target"].Description)
	}
}

func TestWakeupPersistsWithoutDelivery(t *testing.T) {
	_, inbox, factory := setup(t)
	at := time.Now().Add(time.Hour).UTC()
	payload, err := json.Marshal(map[string]any{"target": "thread-a", "at": at, "note": "remember"})
	require.NoError(t, err)
	_, err = factory().Tools()[0].Handler(toolContext(t), payload)
	require.NoError(t, err)
	w := factory()
	cancel, done := start(t, w)
	require.Equal(t, "remember", nextNote(t, inbox, 1))
	items, err := inbox.Active(t.Context())
	require.NoError(t, err)
	require.True(t, items[0].ReadyAt.Equal(at))
	require.Equal(t, "pending", items[0].Status)
	require.Equal(t, domain.ConversationRef("thread-a"), items[0].Payload.Target)
	require.Equal(t, domain.SessionKey("origin"), items[0].SessionKey)
	cancel()
	require.NoError(t, take(t, done))
}

func TestWatchTailAndShutdownGroup(t *testing.T) {
	watches, inbox, factory := setup(t)
	w := factory()
	cancel, done := start(t, w)
	dir := t.TempDir()
	call := func(command string) {
		raw, err := json.Marshal(map[string]string{"target": "thread-a", "command": command, "cwd": dir})
		require.NoError(t, err)
		_, err = w.Tools()[1].Handler(toolContext(t), raw)
		require.NoError(t, err)
	}
	call("printf '%05000d' 0; printf stderr >&2; exit 7")
	note := nextNote(t, inbox, 1)
	prefix := "watch exited with code 7\n"
	require.True(t, strings.HasPrefix(note, prefix))
	require.Len(t, note, len(prefix)+4096)
	require.True(t, strings.HasSuffix(note, "stderr"))
	call("pwd")
	require.Contains(t, nextNote(t, inbox, 2), dir)
	call("sleep 30 & child=$!; echo $child > child.pid; wait")
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "child.pid"))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil
	}, time.Second, time.Millisecond)
	cancel()
	require.NoError(t, take(t, done))
	require.Eventually(t, func() bool {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		return os.IsNotExist(err) || strings.Contains(string(data), ") Z ")
	}, time.Second, time.Millisecond)
	pending, err := watches.Unfinished(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Contains(t, pending[0].Command, "sleep 30")
	next := factory()
	cancel, done = start(t, next)
	require.Contains(t, nextNote(t, inbox, 3), "interrupted by restart; exit unknown")
	pending, err = watches.Unfinished(t.Context())
	require.NoError(t, err)
	require.Empty(t, pending)
	cancel()
	require.NoError(t, take(t, done))
	// A second restart must neither repeat the command nor enqueue another outcome.
	cancel, done = start(t, factory())
	require.Contains(t, nextNote(t, inbox, 3), "interrupted by restart; exit unknown")
	cancel()
	require.NoError(t, take(t, done))
}

func TestFailedSpawnIsDurable(t *testing.T) {
	watches, inbox, factory := setup(t)
	w := factory()
	cancel, done := start(t, w)
	raw, err := json.Marshal(map[string]string{"target": "thread-a", "command": "true", "cwd": filepath.Join(t.TempDir(), "missing")})
	require.NoError(t, err)
	_, err = w.Tools()[1].Handler(toolContext(t), raw)
	require.Error(t, err)
	require.Contains(t, nextNote(t, inbox, 1), "failed to start")
	pending, err := watches.Unfinished(t.Context())
	require.NoError(t, err)
	require.Empty(t, pending)
	cancel()
	require.NoError(t, take(t, done))
}

func TestThreadTargetsSurviveOutOfOrderCompletionAndRestart(t *testing.T) {
	_, inbox, factory := setup(t)
	w := factory()
	cancel, done := start(t, w)
	defer cancel()
	dir := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "gate"), 0600))
	call := func(target, command string) {
		raw, err := json.Marshal(map[string]string{"target": target, "command": command, "cwd": dir})
		require.NoError(t, err)
		_, err = w.Tools()[1].Handler(toolContext(t), raw)
		require.NoError(t, err)
	}
	call("thread-a", "cat gate")
	call("thread-b", "echo SECOND")
	require.Contains(t, nextNote(t, inbox, 1), "SECOND")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gate"), []byte("FIRST"), 0600))
	require.Contains(t, nextNote(t, inbox, 2), "FIRST")
	call("thread-c", "sleep 30")
	cancel()
	require.NoError(t, take(t, done))
	cancel, done = start(t, factory())
	defer cancel()
	require.Contains(t, nextNote(t, inbox, 3), "interrupted by restart")
	items, err := inbox.Active(t.Context())
	require.NoError(t, err)
	for i, target := range []domain.ConversationRef{"thread-b", "thread-a", "thread-c"} {
		require.Equal(t, target, items[i].Payload.Target)
		require.Equal(t, domain.SessionKey("origin"), items[i].SessionKey)
	}
	cancel()
	require.NoError(t, take(t, done))
}
