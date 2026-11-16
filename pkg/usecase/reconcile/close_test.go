package reconcile

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func TestRestClosesAtIdleAndStartsFresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{TurnTimeout: time.Minute})
		f.model.manualAck, f.model.idle = true, true
		f.start(t)
		id := f.enqueue(t, "room", "save memory")
		s := take(t, f.model.opened)
		require.Equal(t, id, take(t, s.inputs).ID)
		f.turn(t, true)
		ctx := domain.WithToolSession(t.Context(), domain.ToolSession{Key: domain.MainSessionKey})
		for range 2 {
			_, err := f.runner.rest(ctx, nil)
			require.NoError(t, err)
			saved, err := f.sessions.Get(t.Context(), domain.MainSessionKey)
			require.NoError(t, err)
			require.Empty(t, saved.ResumeToken)
			require.NotNil(t, saved.Turn)
		}
		synctest.Wait()
		require.Empty(t, s.closed)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		f.turn(t, false)
		synctest.Wait()
		require.Empty(t, s.closed)

		s.busy.Store(true)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
		f.status(t, id, "done")
		next := f.enqueue(t, "room", "input after rest")
		require.Equal(t, next, take(t, s.inputs).ID)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: next}
		f.status(t, next, "done")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		require.Empty(t, f.turn(t, false).ResumeToken)
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		require.Empty(t, s.closed)
		require.Empty(t, f.model.opened)

		s.events <- domain.ModelEvent{Kind: domain.ModelEventBackgroundDone}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnStarted}
		synctest.Wait()
		require.Empty(t, s.closed)
		s.busy.Store(false)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		take(t, s.closed)
		id = f.enqueue(t, "room", "fresh input")
		s = take(t, f.model.opened)
		require.Empty(t, s.config.ResumeToken)
		require.Equal(t, id, take(t, s.inputs).ID)
	})
}

func TestRestStaysFreshAcrossRestartBeforeIdle(t *testing.T) {
	f := newDeliveryRig(t, Config{})
	stop := f.start(t)
	id := f.enqueue(t, "room", "save before restart")
	s := take(t, f.model.opened)
	take(t, s.inputs)
	f.status(t, id, "done")
	_, err := f.runner.rest(domain.WithToolSession(t.Context(), domain.ToolSession{Key: domain.MainSessionKey}), nil)
	require.NoError(t, err)
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	require.Empty(t, f.turn(t, false).ResumeToken)
	stop()
	f.start(t)
	id = f.enqueue(t, "room", "after restart")
	s = take(t, f.model.opened)
	require.Empty(t, s.config.ResumeToken)
	require.Equal(t, id, take(t, s.inputs).ID)
}

func TestRestDispatchesReadyInputBeforeClose(t *testing.T) {
	f := newDeliveryRig(t, Config{})
	r := New(Config{}, f.inbox, repository.NewScheduleRepository(f.db), f.sessions, repository.NewEventRepository(f.db), nil, &hubFake{closed: make(chan domain.SessionKey, 1)})
	p := &process{key: domain.MainSessionKey, closeAtIdle: true, model: &sessionFake{idle: true}, inputs: make(chan domain.InboxItem, 1), cancel: func() {}, turn: &domain.SessionTurn{Deadline: time.Now().Add(time.Hour)}}
	r.live[p.key] = p
	id, err := f.inbox.Enqueue(t.Context(), domain.InboxItem{SourceKey: "ready", SessionKey: "old-room", Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: "arrived before close"}}})
	require.NoError(t, err)
	finish := domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	require.NoError(t, r.handle(t.Context(), result{process: p, event: &finish}))
	_, err = r.pass(t.Context())
	require.NoError(t, err)
	require.False(t, p.closing)
	require.Equal(t, id, take(t, p.inputs).ID)
	require.Equal(t, domain.MainSessionKey, p.delivering.SessionKey)
	require.NoError(t, r.handle(t.Context(), result{process: p, delivered: true}))
	start := domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
	require.NoError(t, r.handle(t.Context(), result{process: p, event: &start}))
	require.NoError(t, r.handle(t.Context(), result{process: p, event: &finish}))
	_, err = r.pass(t.Context())
	require.NoError(t, err)
	require.True(t, p.closing)
	require.Equal(t, "rest", p.reason)
	var status string
	require.NoError(t, f.db.QueryRow("SELECT status FROM inbox WHERE id=?", id).Scan(&status))
	require.Equal(t, "done", status)
}

func TestRestScheduleIsAnOrdinaryProcedureInput(t *testing.T) {
	f := newDeliveryRig(t, Config{Procedure: testProcedure})
	f.model.idle = true
	require.NoError(t, f.sessions.Save(t.Context(), domain.Session{Key: domain.MainSessionKey, ResumeToken: "conversation-token", Context: &domain.SessionContext{Conversation: "room"}}))
	require.NoError(t, repository.NewScheduleRepository(f.db).Save(t.Context(), domain.Schedule{Name: "rest", Cron: "0 4 * * *", TZ: "UTC", NextRun: time.Now()}))
	f.start(t)
	s := take(t, f.model.opened)
	require.Equal(t, domain.MainSessionKey, s.config.Key)
	require.Equal(t, "conversation-token", s.config.ResumeToken)
	input := take(t, s.inputs)
	require.Equal(t, "procedure: rest", input.Text)
	f.status(t, input.ID, "done")
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	require.Equal(t, "resume-token", f.turn(t, false).ResumeToken)
	active, err := f.inbox.Active(t.Context())
	require.NoError(t, err)
	require.Empty(t, active)
	saved, err := f.sessions.Get(t.Context(), domain.MainSessionKey)
	require.NoError(t, err)
	require.Equal(t, "resume-token", saved.ResumeToken)
	require.Empty(t, f.model.opened)
	require.Empty(t, s.closed)
}

func TestRestRequiresAnActiveSession(t *testing.T) {
	r := New(Config{}, nil, nil, nil, nil, nil, nil)
	tool := r.Tools()[2]
	require.Equal(t, "rest", tool.Name)
	_, err := tool.Handler(context.Background(), nil)
	require.ErrorContains(t, err, "session context")
	_, err = tool.Handler(domain.WithToolSession(t.Context(), domain.ToolSession{Key: "missing"}), nil)
	require.ErrorContains(t, err, "no longer active")
}
