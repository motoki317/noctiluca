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
		f.turn(t, "room", true)
		ctx := domain.WithToolSession(t.Context(), domain.ToolSession{Key: "room"})
		for range 2 {
			_, err := f.runner.rest(ctx, nil)
			require.NoError(t, err)
			saved, err := f.sessions.Get(t.Context(), "room")
			require.NoError(t, err)
			require.Empty(t, saved.ResumeToken)
			require.NotNil(t, saved.Turn)
		}
		synctest.Wait()
		require.Empty(t, s.closed)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		f.turn(t, "room", false)
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
		require.Empty(t, f.turn(t, "room", false).ResumeToken)
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
	_, err := f.runner.rest(domain.WithToolSession(t.Context(), domain.ToolSession{Key: "room"}), nil)
	require.NoError(t, err)
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	require.Empty(t, f.turn(t, "room", false).ResumeToken)
	stop()
	f.start(t)
	id = f.enqueue(t, "room", "after restart")
	s = take(t, f.model.opened)
	require.Empty(t, s.config.ResumeToken)
	require.Equal(t, id, take(t, s.inputs).ID)
}

func TestRestScheduleIsAnOrdinaryProcedureInput(t *testing.T) {
	f := newDeliveryRig(t, Config{Procedure: testProcedure})
	f.model.idle = true
	require.NoError(t, f.sessions.Save(t.Context(), domain.Session{Key: "room", ResumeToken: "conversation-token", Context: &domain.SessionContext{Conversation: "room"}}))
	require.NoError(t, repository.NewScheduleRepository(f.db).Save(t.Context(), domain.Schedule{Name: "rest", Cron: "0 4 * * *", TZ: "UTC", NextRun: time.Now()}))
	f.start(t)
	s := take(t, f.model.opened)
	require.Equal(t, domain.SessionKey("task:rest"), s.config.Key)
	input := take(t, s.inputs)
	require.Equal(t, "procedure: rest", input.Text)
	f.status(t, input.ID, "done")
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	require.Equal(t, "resume-token", f.turn(t, "task:rest", false).ResumeToken)
	active, err := f.inbox.Active(t.Context())
	require.NoError(t, err)
	require.Empty(t, active)
	saved, err := f.sessions.Get(t.Context(), "room")
	require.NoError(t, err)
	require.Equal(t, "conversation-token", saved.ResumeToken)
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
