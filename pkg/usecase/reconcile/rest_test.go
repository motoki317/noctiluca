package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func restProcedure(_ context.Context, name string) (string, error) { return "procedure: " + name, nil }

func fireConsolidation(t *testing.T, f *deliveryRig, at time.Time) {
	t.Helper()
	require.NoError(t, repository.NewScheduleRepository(f.db).Save(t.Context(), domain.Schedule{
		Name: "consolidate", Cron: "0 4 * * *", TZ: "UTC", NextRun: at,
	}))
	if f.runner != nil {
		f.runner.Notify()
	}
}

func enqueueRest(t *testing.T, f *deliveryRig) int64 {
	t.Helper()
	id, err := f.inbox.Enqueue(t.Context(), domain.InboxItem{
		SourceKey: "rest:room", SessionKey: "room", Lane: "conv", ReadyAt: time.Now(),
		Payload: domain.InboxPayload{Context: domain.SessionContext{Conversation: "room"}, Procedure: "rest"},
	})
	require.NoError(t, err)
	if f.runner != nil {
		f.runner.Notify()
	}
	return id
}

func TestConsolidationRestsSavedSessionsInTheirLanes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{
			TurnTimeout: 2 * time.Minute, TaskTimeout: time.Minute, Procedure: restProcedure,
			Accept: func(info domain.SessionContext) bool { return info.Conversation != "removed" },
		})
		f.model.idle, f.model.manualAck = true, true
		conversation := domain.SessionContext{Conversation: "room", Direct: true}
		task := domain.SessionContext{TaskName: "daily"}
		for _, saved := range []domain.Session{
			{Key: "conversation", ResumeToken: "conversation-token", Context: &conversation},
			{Key: "task:daily", ResumeToken: "task-token", Context: &task},
			{Key: "task:consolidate", ResumeToken: "consolidate-token", Context: &domain.SessionContext{TaskName: "consolidate"}},
			{Key: "empty", Context: &conversation},
			{Key: "no-context", ResumeToken: "legacy-token"},
			{Key: "rejected", ResumeToken: "rejected-token", Context: &domain.SessionContext{Conversation: "removed"}},
		} {
			require.NoError(t, f.sessions.Save(t.Context(), saved))
		}
		at := time.Now().Add(-time.Minute)
		fireConsolidation(t, f, at)
		f.start(t)
		s := take(t, f.model.opened)
		input := take(t, s.inputs)
		f.status(t, input.ID, "dispatched")
		items, err := f.inbox.Active(t.Context())
		require.NoError(t, err)
		require.Len(t, items, 3)
		contexts := map[domain.SessionKey]domain.SessionContext{"conversation": conversation, "task:daily": task}
		for _, item := range items[:2] {
			info, ok := contexts[item.SessionKey]
			require.True(t, ok)
			require.Equal(t, info, item.Payload.Context)
			require.Equal(t, "rest", item.Payload.Procedure)
			require.Equal(t, fmt.Sprintf("rest:%s:%s", item.SessionKey, at.UTC().Format(time.RFC3339Nano)), item.SourceKey)
			require.True(t, item.ReadyAt.Equal(at))
		}
		require.Equal(t, "consolidate", items[2].Payload.Procedure)
		require.True(t, items[2].ReadyAt.After(at))
		for i, item := range items {
			if i != 0 {
				s = take(t, f.model.opened)
				input = take(t, s.inputs)
			}
			require.Equal(t, item.SessionKey, s.config.Key)
			require.NotEmpty(t, s.config.ResumeToken)
			require.Equal(t, item.ID, input.ID)
			lane, timeout := "task", time.Minute
			if item.SessionKey == "conversation" {
				lane, timeout = "conv", 2*time.Minute
			}
			require.Equal(t, lane, item.Lane)
			require.WithinDuration(t, time.Now().Add(timeout), f.turn(t, string(item.SessionKey), true).Turn.Deadline, 10*time.Millisecond)
			synctest.Wait()
			require.Empty(t, f.model.opened)
			s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: input.ID}
			f.status(t, input.ID, "done")
			s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
			take(t, s.closed)
			require.Empty(t, f.turn(t, string(item.SessionKey), false).ResumeToken)
		}
	})
}

func TestBusyRestAllowsLaterInputsAndConsolidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release domain.ModelEventKind
	}{{"turn", domain.ModelEventTurnFinished}, {"background", domain.ModelEventBackgroundDone}, {"unstarted", domain.ModelEventInputStarted}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryRig(t, Config{Procedure: restProcedure})
				f.model.idle, f.model.manualAck = true, true
				f.start(t)
				first := f.enqueue(t, "room", "work")
				s := take(t, f.model.opened)
				take(t, s.inputs)
				f.turn(t, "room", true)
				if tc.name != "unstarted" {
					s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: first}
					f.status(t, first, "done")
				}
				s.busy.Store(tc.name == "background")
				if tc.name != "turn" {
					s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
					f.turn(t, "room", false)
				}
				fireConsolidation(t, f, time.Now().Add(-time.Minute))
				consolidate := take(t, f.model.opened)
				require.Equal(t, domain.SessionKey("task:consolidate"), consolidate.config.Key)
				input := take(t, consolidate.inputs)
				consolidate.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: input.ID}
				f.status(t, input.ID, "done")
				next := f.enqueue(t, "room", "follow-up")
				require.Equal(t, next, take(t, s.inputs).ID)
				s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: next}
				f.status(t, next, "done")
				if tc.name != "turn" {
					s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
					f.turn(t, "room", false)
				}
				s.busy.Store(false)
				s.events <- domain.ModelEvent{Kind: tc.release, InputID: first}
				synctest.Wait()
				require.Empty(t, s.inputs)
				consolidate.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				take(t, consolidate.closed)
				input = take(t, s.inputs)
				s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: input.ID}
				f.status(t, input.ID, "done")
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				take(t, s.closed)
			})
		})
	}
}

func TestRestDoesNotWaitForFutureWork(t *testing.T) {
	for _, tc := range []struct {
		name       string
		readyAfter time.Duration
		retryAfter time.Duration
	}{
		{name: "ready", readyAfter: 90 * time.Minute, retryAfter: 30 * time.Minute},
		{name: "retry", readyAfter: -2 * time.Minute, retryAfter: 90 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryRig(t, Config{Procedure: restProcedure})
				f.model.idle = true
				info := domain.SessionContext{Conversation: "room"}
				require.NoError(t, f.sessions.Save(t.Context(), domain.Session{Key: "room", ResumeToken: "saved", Context: &info}))
				now := time.Now()
				id, err := f.inbox.Enqueue(t.Context(), domain.InboxItem{
					SourceKey: "future", SessionKey: "room", Lane: "conv",
					ReadyAt: now.Add(tc.readyAfter), RetryAt: now.Add(tc.retryAfter),
					Payload: domain.InboxPayload{Context: info, Input: domain.UserInput{Text: "future work"}},
				})
				require.NoError(t, err)
				fireConsolidation(t, f, now.Add(-time.Minute))
				f.start(t)
				s := take(t, f.model.opened)
				require.Equal(t, domain.SessionKey("room"), s.config.Key)
				require.Equal(t, "saved", s.config.ResumeToken)
				input := take(t, s.inputs)
				require.Equal(t, "procedure: rest", input.Text)
				f.status(t, input.ID, "done")
				f.status(t, id, "pending")
				synctest.Wait()
				require.Empty(t, f.model.opened)
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				take(t, s.closed)
				require.Empty(t, f.turn(t, "room", false).ResumeToken)
				consolidate := take(t, f.model.opened)
				require.Equal(t, domain.SessionKey("task:consolidate"), consolidate.config.Key)
				input = take(t, consolidate.inputs)
				require.Equal(t, "procedure: consolidate", input.Text)
				f.status(t, input.ID, "done")
				consolidate.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				take(t, consolidate.closed)
				time.Sleep(time.Until(now.Add(90*time.Minute - time.Millisecond)))
				synctest.Wait()
				f.status(t, id, "pending")
				require.Empty(t, f.model.opened)
				time.Sleep(time.Millisecond)
				fresh := take(t, f.model.opened)
				require.Equal(t, domain.SessionKey("room"), fresh.config.Key)
				require.Empty(t, fresh.config.ResumeToken)
				require.Equal(t, id, take(t, fresh.inputs).ID)
				f.status(t, id, "done")
			})
		})
	}
}

func TestRestWaitsForRecoveredContinuation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: restProcedure})
		f.model.idle = true
		require.NoError(t, f.sessions.Save(t.Context(), domain.Session{
			Key: "room", ResumeToken: "saved", Context: &domain.SessionContext{Conversation: "room"},
			Turn: &domain.SessionTurn{Deadline: time.Now().Add(time.Minute)},
		}))
		rest := enqueueRest(t, f)
		f.start(t)
		s := take(t, f.model.opened)
		require.Equal(t, "saved", s.config.ResumeToken)
		continuation := take(t, s.inputs)
		require.NotEqual(t, rest, continuation.ID)
		f.status(t, continuation.ID, "done")
		f.status(t, rest, "pending")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		require.Equal(t, rest, take(t, s.inputs).ID)
		f.status(t, rest, "done")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		take(t, s.closed)
		require.Empty(t, f.turn(t, "room", false).ResumeToken)
	})
}

func TestRestHoldsInputsAndTaskSlotUntilProcessExits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: restProcedure})
		release := make(chan struct{})
		f.model.idle, f.model.closeWait = true, release
		stop := f.start(t)
		unblock := sync.OnceFunc(func() { close(release) })
		defer func() { unblock(); stop() }()
		require.NoError(t, f.sessions.Save(t.Context(), domain.Session{Key: "room", ResumeToken: "saved", Context: &domain.SessionContext{Conversation: "room"}}))
		id := enqueueRest(t, f)
		s := take(t, f.model.opened)
		require.Equal(t, "saved", s.config.ResumeToken)
		require.Equal(t, id, take(t, s.inputs).ID)
		f.status(t, id, "done")
		next := f.enqueue(t, "room", "after rest")
		taskID, err := f.inbox.Enqueue(t.Context(), TaskItem("later", "later task", ""))
		require.NoError(t, err)
		f.runner.Notify()
		synctest.Wait()
		require.Empty(t, s.inputs)
		require.Empty(t, f.model.opened)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		take(t, s.closed)
		synctest.Wait()
		require.Empty(t, f.turn(t, "room", false).ResumeToken)
		require.Equal(t, "rest", f.runner.live["room"].reason)
		f.status(t, next, "pending")
		f.status(t, taskID, "pending")
		require.Empty(t, s.inputs)
		require.Empty(t, f.model.opened)
		unblock()
		opened := map[domain.SessionKey]*sessionFake{}
		for range 2 {
			fresh := take(t, f.model.opened)
			opened[fresh.config.Key] = fresh
		}
		fresh := opened["room"]
		require.NotNil(t, fresh)
		require.Empty(t, fresh.config.ResumeToken)
		require.Equal(t, next, take(t, fresh.inputs).ID)
		f.status(t, next, "done")
		require.Equal(t, taskID, take(t, opened["task:later"].inputs).ID)
	})
}

func TestRestRetiresAtTurnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: restProcedure, TurnTimeout: time.Minute})
		f.start(t)
		require.NoError(t, f.sessions.Save(t.Context(), domain.Session{Key: "room", ResumeToken: "saved", Context: &domain.SessionContext{Conversation: "room"}}))
		id := enqueueRest(t, f)
		s := take(t, f.model.opened)
		take(t, s.inputs)
		f.status(t, id, "done")
		deadline := f.turn(t, "room", true).Turn.Deadline
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		f.turn(t, "room", false)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnStarted}
		synctest.Wait()
		require.Nil(t, f.turn(t, "room", false).Turn)
		require.Empty(t, s.closed)
		time.Sleep(time.Until(deadline))
		take(t, s.closed)
		synctest.Wait()
		var continuations int
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'interrupted:%'").Scan(&continuations))
		require.Zero(t, continuations)
	})
}

func TestFailedRestAndConsolidationKeepToken(t *testing.T) {
	for _, procedure := range []string{"rest", "consolidate"} {
		for _, failure := range []string{"error", "late refusal"} {
			t.Run(procedure+"/"+failure, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newDeliveryRig(t, Config{Procedure: restProcedure})
					f.model.idle, f.model.manualAck = true, true
					require.NoError(t, f.sessions.Save(t.Context(), domain.Session{Key: "task:work", ResumeToken: "saved", Context: &domain.SessionContext{TaskName: "work"}}))
					f.start(t)
					item := TaskItem("work", "work", "")
					item.Payload.Procedure = procedure
					id, err := f.inbox.Enqueue(t.Context(), item)
					require.NoError(t, err)
					f.runner.Notify()
					s := take(t, f.model.opened)
					take(t, s.inputs)
					f.turn(t, "task:work", true)
					if failure == "error" {
						s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
						f.status(t, id, "done")
						s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("failed")}
						s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
					} else {
						s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
						f.turn(t, "task:work", false)
						s.events <- domain.ModelEvent{Kind: domain.ModelEventInputRefused, InputID: id, Err: errors.New("busy")}
						f.status(t, id, "pending")
					}
					synctest.Wait()
					require.Equal(t, "resume-token", f.turn(t, "task:work", false).ResumeToken)
					require.Empty(t, s.closed)
					if failure == "error" {
						return
					}
					if procedure == "rest" {
						next, err := f.inbox.Enqueue(t.Context(), TaskItem("work", "during retry", ""))
						require.NoError(t, err)
						f.runner.Notify()
						require.Equal(t, next, take(t, s.inputs).ID)
						s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: next}
						f.status(t, next, "done")
						s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
						f.turn(t, "task:work", false)
					}
					time.Sleep(2 * time.Second)
					require.Equal(t, id, take(t, s.inputs).ID)
					s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
					f.status(t, id, "done")
					s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
					take(t, s.closed)
					require.Empty(t, f.turn(t, "task:work", false).ResumeToken)
					require.Empty(t, f.model.opened)
				})
			})
		}
	}
}

func TestRestTokenResetSurvivesRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: restProcedure})
		f.model.manualAck = true
		stop := f.start(t)
		require.NoError(t, f.sessions.Save(t.Context(), domain.Session{Key: "room", ResumeToken: "saved", Context: &domain.SessionContext{Conversation: "room"}}))
		id := enqueueRest(t, f)
		s := take(t, f.model.opened)
		take(t, s.inputs)
		f.turn(t, "room", true)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		require.Empty(t, f.turn(t, "room", false).ResumeToken)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
		f.status(t, id, "done")
		require.Empty(t, f.turn(t, "room", false).ResumeToken)
		stop()
		f.start(t)
		next := f.enqueue(t, "room", "after restart")
		fresh := take(t, f.model.opened)
		require.Empty(t, fresh.config.ResumeToken)
		require.Equal(t, next, take(t, fresh.inputs).ID)
	})
}

func TestRepeatedNightsSkipAlreadyRestedSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: restProcedure, TurnTimeout: 72 * time.Hour, IdleTimeout: 72 * time.Hour})
		f.model.idle = true
		f.start(t)
		id := f.enqueue(t, "room", "long-running work")
		s := take(t, f.model.opened)
		take(t, s.inputs)
		f.status(t, id, "done")
		at := time.Now().Add(-time.Minute)
		fireConsolidation(t, f, at)
		for night := range 2 {
			if night != 0 {
				schedules, err := repository.NewScheduleRepository(f.db).List(t.Context())
				require.NoError(t, err)
				time.Sleep(time.Until(schedules[0].NextRun))
			}
			consolidate := take(t, f.model.opened)
			require.Equal(t, domain.SessionKey("task:consolidate"), consolidate.config.Key)
			input := take(t, consolidate.inputs)
			f.status(t, input.ID, "done")
			consolidate.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
			take(t, consolidate.closed)
		}
		items, err := f.inbox.Active(t.Context())
		require.NoError(t, err)
		require.Len(t, items, 2)
		require.Equal(t, "rest", items[0].Payload.Procedure)
		require.Equal(t, "rest", items[1].Payload.Procedure)
		_, err = f.db.Exec("UPDATE schedules SET next_run=? WHERE name='consolidate'", items[1].ReadyAt.UTC())
		require.NoError(t, err)
		f.runner.Notify()
		synctest.Wait()
		var count int
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox").Scan(&count))
		require.Equal(t, 5, count)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		input := take(t, s.inputs)
		f.status(t, input.ID, "done")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		take(t, s.closed)
		for _, item := range items {
			f.status(t, item.ID, "done")
		}
		synctest.Wait()
		require.Empty(t, f.model.opened)
		require.Empty(t, s.inputs)
		require.Empty(t, f.turn(t, "room", false).ResumeToken)
	})
}

func TestEmptyRestProcedureKeepsToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: func(ctx context.Context, name string) (string, error) {
			if name == "rest" {
				return "", nil
			}
			return restProcedure(ctx, name)
		}})
		f.model.idle = true
		require.NoError(t, f.sessions.Save(t.Context(), domain.Session{Key: "room", ResumeToken: "saved", Context: &domain.SessionContext{Conversation: "room"}}))
		at := time.Now().Add(-time.Minute)
		fireConsolidation(t, f, at)
		f.start(t)
		s := take(t, f.model.opened)
		require.Equal(t, domain.SessionKey("task:consolidate"), s.config.Key)
		input := take(t, s.inputs)
		f.status(t, input.ID, "done")
		require.Equal(t, "saved", f.turn(t, "room", false).ResumeToken)
		items, err := f.inbox.Active(t.Context())
		require.NoError(t, err)
		require.Empty(t, items)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		take(t, s.closed)
		require.Empty(t, f.model.opened)
	})
}

func TestUserScheduleNamedRestKeepsItsPromptAndToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: func(context.Context, string) (string, error) {
			t.Error("user schedule must not read a procedure")
			return "", nil
		}})
		f.model.idle = true
		f.start(t)
		_, err := f.runner.Tools()[0].Handler(t.Context(), []byte(`{"name":"rest","cron":"0 4 * * *","tz":"UTC","prompt":"user prompt"}`))
		require.NoError(t, err)
		_, err = f.db.Exec("UPDATE schedules SET next_run=? WHERE name='rest'", time.Now().Add(-time.Minute).UTC())
		require.NoError(t, err)
		f.runner.Notify()
		s := take(t, f.model.opened)
		require.Equal(t, domain.SessionKey("task:rest"), s.config.Key)
		input := take(t, s.inputs)
		require.Equal(t, "user prompt", input.Text)
		f.status(t, input.ID, "done")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		require.Equal(t, "resume-token", f.turn(t, "task:rest", false).ResumeToken)
		synctest.Wait()
		require.Empty(t, s.closed)
	})
}
