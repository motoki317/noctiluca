package repository_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func inboxItem(source string) domain.InboxItem {
	return domain.InboxItem{SourceKey: source, SessionKey: "room", Actor: domain.ActorMember, Lane: "conv", ReadyAt: time.Now().UTC(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: "hello"}}}
}
func TestInboxAtomicReceiveAndRetry(t *testing.T) {
	db := openDB(t)
	r := repository.NewInboxRepository(db)
	item := inboxItem("event:1")
	event := domain.InboundEvent{Kind: domain.InboundEventMessage, Conversation: "room", Sender: domain.Sender{ID: "member", Kind: domain.SenderHuman}, ReceivedAt: item.ReadyAt}
	require.NoError(t, r.Receive(t.Context(), event, item))
	require.NoError(t, r.Receive(t.Context(), event, item))
	events, err := repository.NewEventRepository(db).ListInbound(t.Context(), "room", 10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	item.SourceKey = "event:bad"
	event.Kind = "invalid"
	require.Error(t, r.Receive(t.Context(), event, item))
	items, err := r.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 1)
	deadline := time.Now().Add(time.Minute).UTC()
	ok, err := r.Claim(t.Context(), items[0].ID, deadline)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = r.Claim(t.Context(), items[0].ID, deadline)
	require.NoError(t, err)
	require.False(t, ok)
	retry := time.Now().Add(time.Second).UTC()
	dead, err := r.Finish(t.Context(), items[0].ID, "model failed", 2, retry)
	require.NoError(t, err)
	require.False(t, dead)
	items, err = r.Active(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, items[0].Attempts)
	require.Equal(t, retry, items[0].RetryAt)
	_, err = r.Claim(t.Context(), items[0].ID, deadline)
	require.NoError(t, err)
	dead, err = r.Finish(t.Context(), items[0].ID, "model failed again", 2, retry)
	require.NoError(t, err)
	require.True(t, dead)
	dead, err = r.Finish(t.Context(), items[0].ID, "duplicate callback", 2, retry)
	require.NoError(t, err)
	require.False(t, dead)
}
func TestInboxRestartAndWatchOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, err := repository.New(t.Context(), repository.Config{Path: path})
	require.NoError(t, err)
	r := repository.NewInboxRepository(db)
	first, err := r.Enqueue(t.Context(), inboxItem("one"))
	require.NoError(t, err)
	_, err = r.Enqueue(t.Context(), inboxItem("two"))
	require.NoError(t, err)
	_, err = r.Claim(t.Context(), first, time.Now().Add(time.Minute))
	require.NoError(t, err)
	watches := repository.NewWatchRepository(db)
	id, err := watches.Add(t.Context(), domain.Watch{Command: "do work", CWD: "/work", Item: inboxItem("")})
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = repository.New(t.Context(), repository.Config{Path: path})
	require.NoError(t, err)
	defer db.Close()
	r = repository.NewInboxRepository(db)
	items, err := r.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, "dispatched", items[0].Status)
	require.Equal(t, "pending", items[1].Status)
	watches = repository.NewWatchRepository(db)
	unfinished, err := watches.Unfinished(t.Context())
	require.NoError(t, err)
	require.Len(t, unfinished, 1)
	require.NoError(t, watches.Complete(t.Context(), id, "interrupted by restart; exit unknown", time.Now()))
	require.NoError(t, watches.Complete(t.Context(), id, "duplicate", time.Now()))
	unfinished, err = watches.Unfinished(t.Context())
	require.NoError(t, err)
	require.Empty(t, unfinished)
	items, err = r.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 3)
	require.Equal(t, "interrupted by restart; exit unknown", items[2].Payload.Input.Text)
}
func TestScheduleFireIsAtomicAndCoalesces(t *testing.T) {
	db := openDB(t)
	r := repository.NewInboxRepository(db)
	schedules := repository.NewScheduleRepository(db)
	s := domain.Schedule{Name: "daily", Cron: "0 4 * * *", TZ: "UTC", NextRun: time.Now().Add(-48 * time.Hour).UTC()}
	require.NoError(t, schedules.Save(t.Context(), s))
	next := time.Now().Add(time.Hour).UTC()
	item := inboxItem("schedule:daily:old")
	invalid := item
	invalid.Lane = "invalid"
	require.Error(t, r.Fire(t.Context(), s, next, &invalid))
	saved, err := schedules.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, s.NextRun, saved[0].NextRun)
	require.NoError(t, r.Fire(t.Context(), s, next, &item))
	require.NoError(t, r.Fire(t.Context(), s, next, &item))
	items, err := r.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 1)
	saved, err = schedules.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, next, saved[0].NextRun)
}

func TestScheduleTimingUpdate(t *testing.T) {
	r := repository.NewScheduleRepository(openDB(t))
	s := domain.Schedule{Name: "daily", Cron: "0 4 * * *", TZ: "UTC", NextRun: time.Now().UTC()}
	require.NoError(t, r.Save(t.Context(), s))
	original := s.NextRun
	s.NextRun = s.NextRun.Add(time.Hour)
	require.NoError(t, r.Save(t.Context(), s))
	saved, err := r.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, original, saved[0].NextRun)
	s.Cron = "* * * * *"
	require.NoError(t, r.Save(t.Context(), s))
	saved, err = r.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, s.NextRun, saved[0].NextRun)
}
