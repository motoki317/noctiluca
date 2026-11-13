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

func TestConsolidationRestsOnlyConversationSessions(t *testing.T) {
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
		require.Len(t, items, 2)
		for _, item := range items[:1] {
			require.Equal(t, domain.SessionKey("conversation"), item.SessionKey)
			require.Equal(t, conversation, item.Payload.Context)
			require.Equal(t, "rest", item.Payload.Procedure)
			require.Equal(t, fmt.Sprintf("rest:%s:%s", item.SessionKey, at.UTC().Format(time.RFC3339Nano)), item.SourceKey)
			require.True(t, item.ReadyAt.Equal(at))
		}
		require.Equal(t, "consolidate", items[1].Payload.Procedure)
		require.True(t, items[1].ReadyAt.After(at))
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
		require.Equal(t, "task-token", f.turn(t, "task:daily", false).ResumeToken)
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
					var next int64
					if procedure == "rest" {
						next, err = f.inbox.Enqueue(t.Context(), TaskItem("work", "during retry", ""))
						require.NoError(t, err)
						f.runner.Notify()
						synctest.Wait()
						require.Empty(t, s.inputs)
						f.status(t, next, "pending")
					}
					time.Sleep(2 * time.Second)
					require.Equal(t, id, take(t, s.inputs).ID)
					s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
					f.status(t, id, "done")
					s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
					take(t, s.closed)
					require.Empty(t, f.turn(t, "task:work", false).ResumeToken)
					if next != 0 {
						fresh := take(t, f.model.opened)
						require.Empty(t, fresh.config.ResumeToken)
						require.Equal(t, next, take(t, fresh.inputs).ID)
						fresh.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: next}
						f.status(t, next, "done")
					} else {
						require.Empty(t, f.model.opened)
					}
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

func installTaskRestSchedules(t *testing.T, f *deliveryRig, cron string) time.Time {
	t.Helper()
	now := time.Now()
	consolidate := domain.Schedule{Name: "consolidate", Cron: "0 4 * * *", TZ: "UTC"}
	boundary, err := Next(consolidate, now)
	require.NoError(t, err)
	consolidate.NextRun = boundary
	schedules := repository.NewScheduleRepository(f.db)
	require.NoError(t, schedules.Save(t.Context(), consolidate))
	require.NoError(t, schedules.Save(t.Context(), domain.Schedule{
		Name: "daily", Cron: cron, TZ: "UTC", Prompt: "daily work", NextRun: now,
	}))
	return boundary
}

func finishTaskRest(t *testing.T, f *deliveryRig, s *sessionFake) {
	t.Helper()
	input := take(t, s.inputs)
	require.Equal(t, "procedure: rest", input.Text)
	f.status(t, input.ID, "done")
	synctest.Wait()
	require.Empty(t, f.model.opened)
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	take(t, s.closed)
	require.Empty(t, f.turn(t, string(s.config.Key), false).ResumeToken)
}

func TestLastTaskFireRestsInSameProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: restProcedure})
		f.model.idle = true
		installTaskRestSchedules(t, f, "0 0 * * *")
		f.start(t)
		s := take(t, f.model.opened)
		require.Equal(t, domain.SessionKey("task:daily"), s.config.Key)
		input := take(t, s.inputs)
		f.status(t, input.ID, "done")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		finishTaskRest(t, f, s)
		_, err := f.db.Exec("UPDATE schedules SET next_run=? WHERE name='daily'", time.Now().Add(time.Minute).UTC())
		require.NoError(t, err)
		f.runner.Notify()
		time.Sleep(time.Minute)
		fresh := take(t, f.model.opened)
		require.Equal(t, s.config.Key, fresh.config.Key)
		require.Empty(t, fresh.config.ResumeToken)
		require.Equal(t, "daily work", take(t, fresh.inputs).Text)
	})
}

func TestEarlierTaskFireKeepsTokenUntilLastFire(t *testing.T) {
	for _, idleTimeout := range []time.Duration{time.Hour, 3 * time.Hour} {
		t.Run(idleTimeout.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryRig(t, Config{Procedure: restProcedure, IdleTimeout: idleTimeout})
				f.model.idle = true
				installTaskRestSchedules(t, f, "0 0,2 * * *")
				f.start(t)
				s := take(t, f.model.opened)
				input := take(t, s.inputs)
				f.status(t, input.ID, "done")
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				require.Equal(t, "resume-token", f.turn(t, "task:daily", false).ResumeToken)
				synctest.Wait()
				require.Empty(t, s.inputs)
				time.Sleep(2 * time.Hour)
				if idleTimeout < 2*time.Hour {
					fresh := take(t, f.model.opened)
					require.Equal(t, s.config.Key, fresh.config.Key)
					require.Equal(t, "resume-token", fresh.config.ResumeToken)
					s = fresh
				} else {
					require.Empty(t, f.model.opened)
				}
				input = take(t, s.inputs)
				require.Equal(t, "daily work", input.Text)
				f.status(t, input.ID, "done")
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				finishTaskRest(t, f, s)
			})
		})
	}
}

func TestLastTaskFireRestsAfterBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: restProcedure, TaskTimeout: 6 * time.Hour})
		f.model.idle = true
		boundary := installTaskRestSchedules(t, f, "0 0 * * *")
		f.start(t)
		s := take(t, f.model.opened)
		input := take(t, s.inputs)
		f.status(t, input.ID, "done")
		time.Sleep(time.Until(boundary.Add(time.Minute)))
		synctest.Wait()
		require.Empty(t, s.inputs)
		schedules, err := repository.NewScheduleRepository(f.db).List(t.Context())
		require.NoError(t, err)
		for _, schedule := range schedules {
			require.True(t, schedule.NextRun.After(time.Now()))
		}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		finishTaskRest(t, f, s)
		consolidate := take(t, f.model.opened)
		require.Equal(t, domain.SessionKey("task:consolidate"), consolidate.config.Key)
		require.Equal(t, "procedure: consolidate", take(t, consolidate.inputs).Text)
	})
}

func TestTaskRestRequiresConsolidationAndProcedure(t *testing.T) {
	for _, missing := range []string{"consolidation", "procedure"} {
		t.Run(missing, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryRig(t, Config{Procedure: restProcedure})
				f.model.idle = true
				installTaskRestSchedules(t, f, "0 0 * * *")
				if missing == "consolidation" {
					require.NoError(t, repository.NewScheduleRepository(f.db).Delete(t.Context(), "consolidate"))
				} else {
					f.config.Procedure = nil
				}
				f.start(t)
				s := take(t, f.model.opened)
				input := take(t, s.inputs)
				f.status(t, input.ID, "done")
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				require.Equal(t, "resume-token", f.turn(t, "task:daily", false).ResumeToken)
				synctest.Wait()
				require.Empty(t, s.inputs)
				require.Empty(t, s.closed)
				items, err := f.inbox.Active(t.Context())
				require.NoError(t, err)
				require.Empty(t, items)
			})
		})
	}
}

func TestTaskRestWaitsForFollowUp(t *testing.T) {
	for _, followUp := range []string{"wake", "wake after idle", "watch", "background", "dispatched"} {
		t.Run(followUp, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryRig(t, Config{Procedure: restProcedure, IdleTimeout: 8 * time.Hour})
				if followUp == "wake after idle" {
					f.config.IdleTimeout = time.Hour
				}
				f.model.idle = true
				boundary := installTaskRestSchedules(t, f, "0 0 * * *")
				f.start(t)
				s := take(t, f.model.opened)
				input := take(t, s.inputs)
				f.status(t, input.ID, "done")
				item := TaskItem("daily", "follow-up", "")
				item.SourceKey = "follow-up"
				var watchID, next int64
				var err error
				watches := repository.NewWatchRepository(f.db)
				switch followUp {
				case "watch":
					watchID, err = watches.Add(t.Context(), domain.Watch{Command: "work", Item: item})
				case "background":
					s.busy.Store(true)
				default:
					item.ReadyAt = boundary.Add(time.Minute)
					next, err = f.inbox.Enqueue(t.Context(), item)
					if followUp == "dispatched" {
						claimed, claimErr := f.inbox.Claim(t.Context(), next, boundary.Add(time.Hour))
						require.NoError(t, claimErr)
						require.True(t, claimed)
					}
				}
				require.NoError(t, err)
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				require.Equal(t, "resume-token", f.turn(t, "task:daily", false).ResumeToken)
				synctest.Wait()
				require.Empty(t, s.inputs)
				if followUp == "background" {
					s.busy.Store(false)
					s.events <- domain.ModelEvent{Kind: domain.ModelEventBackgroundDone}
				} else {
					time.Sleep(time.Until(boundary))
					consolidate := take(t, f.model.opened)
					require.Equal(t, domain.SessionKey("task:consolidate"), consolidate.config.Key)
					input = take(t, consolidate.inputs)
					f.status(t, input.ID, "done")
					consolidate.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
					take(t, consolidate.closed)
					switch followUp {
					case "watch":
						require.NoError(t, watches.Complete(t.Context(), watchID, "follow-up", time.Now().Add(time.Minute)))
						f.runner.Notify()
					case "dispatched":
						_, err = f.db.Exec("UPDATE inbox SET status='pending' WHERE id=?", next)
						require.NoError(t, err)
						f.runner.Notify()
					}
					time.Sleep(time.Minute)
					if followUp == "wake after idle" {
						take(t, s.closed)
						s = take(t, f.model.opened)
						require.Equal(t, "resume-token", s.config.ResumeToken)
					}
					input = take(t, s.inputs)
					require.Equal(t, "follow-up", input.Text)
					f.status(t, input.ID, "done")
					synctest.Wait()
					require.Empty(t, s.inputs)
					s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				}
				finishTaskRest(t, f, s)
			})
		})
	}
}

func TestTaskRestPrecedesOtherDueTasks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: restProcedure})
		f.model.idle = true
		installTaskRestSchedules(t, f, "0 0 * * *")
		f.start(t)
		s := take(t, f.model.opened)
		input := take(t, s.inputs)
		f.status(t, input.ID, "done")
		id, err := f.inbox.Enqueue(t.Context(), TaskItem("other", "other task", ""))
		require.NoError(t, err)
		f.runner.Notify()
		synctest.Wait()
		f.status(t, id, "pending")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		finishTaskRest(t, f, s)
		other := take(t, f.model.opened)
		require.Equal(t, domain.SessionKey("task:other"), other.config.Key)
		require.Equal(t, id, take(t, other.inputs).ID)
	})
}

func TestQueuedTaskRestKeepsWarmProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: restProcedure, IdleTimeout: time.Minute, TaskTimeout: 10 * time.Minute})
		f.model.idle = true
		installTaskRestSchedules(t, f, "0 0 * * *")
		f.start(t)
		s := take(t, f.model.opened)
		input := take(t, s.inputs)
		f.status(t, input.ID, "done")
		s.busy.Store(true)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		f.turn(t, "task:daily", false)
		id, err := f.inbox.Enqueue(t.Context(), TaskItem("other", "other task", ""))
		require.NoError(t, err)
		f.runner.Notify()
		other := take(t, f.model.opened)
		require.Equal(t, id, take(t, other.inputs).ID)
		f.status(t, id, "done")
		s.busy.Store(false)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventBackgroundDone}
		synctest.Wait()
		items, err := f.inbox.Active(t.Context())
		require.NoError(t, err)
		require.Len(t, items, 1)
		require.Equal(t, "rest", items[0].Payload.Procedure)
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		require.Empty(t, s.closed)
		require.Empty(t, s.inputs)
		require.Empty(t, f.model.opened)
		other.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		finishTaskRest(t, f, s)
	})
}

func TestEmptyTaskRestDoesNotLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: func(context.Context, string) (string, error) { return "", nil }})
		f.model.idle = true
		installTaskRestSchedules(t, f, "0 0 * * *")
		f.start(t)
		s := take(t, f.model.opened)
		input := take(t, s.inputs)
		f.status(t, input.ID, "done")
		for turn := range 2 {
			s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
			f.turn(t, "task:daily", false)
			synctest.Wait()
			require.Empty(t, s.inputs)
			require.Equal(t, "resume-token", f.turn(t, "task:daily", false).ResumeToken)
			var count int
			require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'rest:task:daily:%'").Scan(&count))
			require.Equal(t, turn+1, count)
			if turn == 0 {
				id, err := f.inbox.Enqueue(t.Context(), TaskItem("daily", "follow-up", ""))
				require.NoError(t, err)
				f.runner.Notify()
				require.Equal(t, id, take(t, s.inputs).ID)
				f.status(t, id, "done")
			}
		}
	})
}

func TestHeartbeatEndsWithoutRest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: restProcedure})
		f.model.idle = true
		f.start(t)
		for fire := range 2 {
			item := TaskItem("heartbeat", "heartbeat", "")
			item.Payload.Procedure = "heartbeat"
			id, err := f.inbox.Enqueue(t.Context(), item)
			require.NoError(t, err)
			f.runner.Notify()
			s := take(t, f.model.opened)
			require.Empty(t, s.config.ResumeToken)
			require.Equal(t, id, take(t, s.inputs).ID)
			f.status(t, id, "done")
			if fire == 0 {
				s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("heartbeat failed")}
			}
			s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
			take(t, s.closed)
			require.Empty(t, f.turn(t, "task:heartbeat", false).ResumeToken)
			require.Empty(t, s.inputs)
		}
		var count int
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'rest:%'").Scan(&count))
		require.Zero(t, count)
	})
}

func TestHeartbeatFollowUpEndsWithoutRest(t *testing.T) {
	for _, followUp := range []string{"wake", "watch", "continuation"} {
		t.Run(followUp, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryRig(t, Config{Procedure: restProcedure})
				f.model.idle = true
				installTaskRestSchedules(t, f, "0 0 * * *")
				schedules := repository.NewScheduleRepository(f.db)
				require.NoError(t, schedules.Delete(t.Context(), "daily"))
				require.NoError(t, schedules.Save(t.Context(), domain.Schedule{
					Name: "heartbeat", Cron: "0 0 * * *", TZ: "UTC", NextRun: time.Now(),
				}))
				f.start(t)
				s := take(t, f.model.opened)
				require.Equal(t, domain.SessionKey("task:heartbeat"), s.config.Key)
				input := take(t, s.inputs)
				f.status(t, input.ID, "done")
				item := TaskItem("heartbeat", "follow-up", "")
				item.ReadyAt = time.Now().Add(time.Minute)
				if followUp == "watch" {
					watches := repository.NewWatchRepository(f.db)
					id, err := watches.Add(t.Context(), domain.Watch{Command: "work", Item: item})
					require.NoError(t, err)
					require.NoError(t, watches.Complete(t.Context(), id, "follow-up", item.ReadyAt))
				} else {
					if followUp == "continuation" {
						item.Payload.Continuations = 1
					}
					_, err := f.inbox.Enqueue(t.Context(), item)
					require.NoError(t, err)
				}
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				take(t, s.closed)
				require.Empty(t, f.turn(t, "task:heartbeat", false).ResumeToken)
				time.Sleep(time.Minute)
				fresh := take(t, f.model.opened)
				require.Empty(t, fresh.config.ResumeToken)
				input = take(t, fresh.inputs)
				require.Equal(t, "follow-up", input.Text)
				f.status(t, input.ID, "done")
				if followUp == "continuation" {
					fresh.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("follow-up failed")}
				}
				fresh.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				synctest.Wait()
				require.Empty(t, fresh.inputs)
				take(t, fresh.closed)
				require.Empty(t, f.turn(t, "task:heartbeat", false).ResumeToken)
				var rests int
				require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'rest:task:heartbeat:%'").Scan(&rests))
				require.Zero(t, rests)
				_, err := f.db.Exec("UPDATE schedules SET next_run=? WHERE name='heartbeat'", time.Now().Add(time.Minute).UTC())
				require.NoError(t, err)
				f.runner.Notify()
				time.Sleep(time.Minute)
				next := take(t, f.model.opened)
				require.Equal(t, fresh.config.Key, next.config.Key)
				require.Empty(t, next.config.ResumeToken)
				require.Equal(t, "procedure: heartbeat", take(t, next.inputs).Text)
			})
		})
	}
}

func TestHeartbeatLateRefusalKeepsTokenEmpty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: restProcedure})
		f.model.idle, f.model.manualAck = true, true
		installTaskRestSchedules(t, f, "0 0 * * *")
		schedules := repository.NewScheduleRepository(f.db)
		require.NoError(t, schedules.Delete(t.Context(), "daily"))
		require.NoError(t, schedules.Save(t.Context(), domain.Schedule{
			Name: "heartbeat", Cron: "0 0 * * *", TZ: "UTC", NextRun: time.Now(),
		}))
		f.start(t)
		s := take(t, f.model.opened)
		input := take(t, s.inputs)
		f.turn(t, "task:heartbeat", true)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		require.Empty(t, f.turn(t, "task:heartbeat", false).ResumeToken)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputRefused, InputID: input.ID, Err: errors.New("busy")}
		f.status(t, input.ID, "pending")
		synctest.Wait()
		require.Empty(t, f.turn(t, "task:heartbeat", false).ResumeToken)
		take(t, s.closed)
		time.Sleep(2 * time.Second)
		fresh := take(t, f.model.opened)
		require.Empty(t, fresh.config.ResumeToken)
		require.Equal(t, input.ID, take(t, fresh.inputs).ID)
	})
}

func TestBoundaryWakeKeepsNewFireContext(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty rest=%t", empty), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				time.Sleep(3*time.Hour + 45*time.Minute)
				procedure := restProcedure
				if empty {
					procedure = func(_ context.Context, name string) (string, error) {
						if name == "rest" {
							return "", nil
						}
						return restProcedure(t.Context(), name)
					}
				}
				f := newDeliveryRig(t, Config{Procedure: procedure})
				f.model.idle = true
				boundary := installTaskRestSchedules(t, f, "*/15 * * * *")
				f.start(t)
				s := take(t, f.model.opened)
				input := take(t, s.inputs)
				f.status(t, input.ID, "done")
				wake := TaskItem("daily", "boundary wake", "")
				wake.ReadyAt = boundary
				id, err := f.inbox.Enqueue(t.Context(), wake)
				require.NoError(t, err)
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				if empty {
					f.turn(t, "task:daily", false)
					synctest.Wait()
					require.Empty(t, s.inputs)
				} else {
					finishTaskRest(t, f, s)
				}
				time.Sleep(time.Until(boundary))
				fresh := s
				if !empty {
					fresh = take(t, f.model.opened)
				}
				require.Equal(t, s.config.Key, fresh.config.Key)
				require.Equal(t, id, take(t, fresh.inputs).ID)
				if !empty {
					require.Empty(t, fresh.config.ResumeToken)
				}
				f.status(t, id, "done")
				fresh.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				synctest.Wait()
				var rests int
				require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'rest:task:daily:%'").Scan(&rests))
				require.Equal(t, 1, rests)
				consolidate := take(t, f.model.opened)
				require.Equal(t, domain.SessionKey("task:consolidate"), consolidate.config.Key)
				input = take(t, consolidate.inputs)
				f.status(t, input.ID, "done")
				consolidate.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				take(t, consolidate.closed)
				require.Equal(t, "daily work", take(t, fresh.inputs).Text)
			})
		})
	}
}

func TestDelayedWakeKeepsLastFire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(3*time.Hour + 45*time.Minute)
		f := newDeliveryRig(t, Config{Procedure: restProcedure, TaskTimeout: time.Hour})
		f.model.idle = true
		boundary := installTaskRestSchedules(t, f, "*/15 * * * *")
		f.start(t)
		s := take(t, f.model.opened)
		input := take(t, s.inputs)
		f.status(t, input.ID, "done")
		wake := TaskItem("daily", "delayed wake", "")
		wake.ReadyAt = boundary.Add(-time.Minute)
		id, err := f.inbox.Enqueue(t.Context(), wake)
		require.NoError(t, err)
		otherID, err := f.inbox.Enqueue(t.Context(), TaskItem("other", "other work", ""))
		require.NoError(t, err)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		other := take(t, f.model.opened)
		require.Equal(t, otherID, take(t, other.inputs).ID)
		f.status(t, otherID, "done")
		time.Sleep(time.Until(boundary.Add(time.Minute)))
		other.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		require.Equal(t, id, take(t, s.inputs).ID)
		f.status(t, id, "done")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		finishTaskRest(t, f, s)
		consolidate := take(t, f.model.opened)
		require.Equal(t, domain.SessionKey("task:consolidate"), consolidate.config.Key)
		input = take(t, consolidate.inputs)
		f.status(t, input.ID, "done")
		consolidate.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		take(t, consolidate.closed)
		fresh := take(t, f.model.opened)
		require.Equal(t, s.config.Key, fresh.config.Key)
		require.Empty(t, fresh.config.ResumeToken)
		require.Equal(t, "daily work", take(t, fresh.inputs).Text)
	})
}

func TestTaskRestIgnoresLaterWork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ready   time.Duration
		retry   time.Duration
		removed bool
	}{
		{name: "at next fire", ready: 24 * time.Hour},
		{name: "after next fire", ready: 25 * time.Hour},
		{name: "retry after next fire", retry: 25 * time.Hour},
		{name: "removed schedule", ready: time.Minute, removed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryRig(t, Config{Procedure: restProcedure})
				f.model.idle = true
				installTaskRestSchedules(t, f, "0 0 * * *")
				f.start(t)
				s := take(t, f.model.opened)
				input := take(t, s.inputs)
				f.status(t, input.ID, "done")
				var nextFire time.Time
				require.NoError(t, f.db.QueryRow("SELECT next_run FROM schedules WHERE name='daily'").Scan(&nextFire))
				fire := nextFire.Add(-24 * time.Hour)
				item := TaskItem("daily", "later work", "")
				item.ReadyAt = fire.Add(tc.ready)
				item.RetryAt = fire.Add(tc.retry)
				id, err := f.inbox.Enqueue(t.Context(), item)
				require.NoError(t, err)
				if tc.removed {
					require.NoError(t, repository.NewScheduleRepository(f.db).Delete(t.Context(), "daily"))
				}
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				finishTaskRest(t, f, s)
				f.status(t, id, "pending")
				if tc.removed {
					time.Sleep(time.Minute)
					fresh := take(t, f.model.opened)
					require.Empty(t, fresh.config.ResumeToken)
					require.Equal(t, id, take(t, fresh.inputs).ID)
					f.status(t, id, "done")
					fresh.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
					finishTaskRest(t, f, fresh)
				}
			})
		})
	}
}

func TestTaskRestPrecedesItsNextFire(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued=%t", queued), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				time.Sleep(3*time.Hour + 45*time.Minute)
				f := newDeliveryRig(t, Config{Procedure: restProcedure, TaskTimeout: time.Hour, IdleTimeout: time.Minute})
				f.model.idle = true
				boundary := installTaskRestSchedules(t, f, "*/15 * * * *")
				f.start(t)
				s := take(t, f.model.opened)
				input := take(t, s.inputs)
				f.status(t, input.ID, "done")
				var other *sessionFake
				if queued {
					s.busy.Store(true)
					s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
					f.turn(t, "task:daily", false)
					id, err := f.inbox.Enqueue(t.Context(), TaskItem("other", "other work", ""))
					require.NoError(t, err)
					f.runner.Notify()
					other = take(t, f.model.opened)
					require.Equal(t, id, take(t, other.inputs).ID)
					f.status(t, id, "done")
					s.busy.Store(false)
					s.events <- domain.ModelEvent{Kind: domain.ModelEventBackgroundDone}
					synctest.Wait()
					items, err := f.inbox.Active(t.Context())
					require.NoError(t, err)
					require.Len(t, items, 1)
					require.Equal(t, "rest", items[0].Payload.Procedure)
				}
				time.Sleep(time.Until(boundary.Add(time.Minute)))
				if queued {
					other.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				} else {
					s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				}
				finishTaskRest(t, f, s)
				consolidate := take(t, f.model.opened)
				require.Equal(t, domain.SessionKey("task:consolidate"), consolidate.config.Key)
				input = take(t, consolidate.inputs)
				f.status(t, input.ID, "done")
				consolidate.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				take(t, consolidate.closed)
				fresh := take(t, f.model.opened)
				require.Equal(t, s.config.Key, fresh.config.Key)
				require.Empty(t, fresh.config.ResumeToken)
				input = take(t, fresh.inputs)
				require.Equal(t, "daily work", input.Text)
				f.status(t, input.ID, "done")
				fresh.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				require.Equal(t, "resume-token", f.turn(t, "task:daily", false).ResumeToken)
				synctest.Wait()
				require.Empty(t, fresh.inputs)
			})
		})
	}
}

func TestLastTaskFireBoundaryAndRemovedSchedule(t *testing.T) {
	fire := time.Date(2026, time.November, 13, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		task *domain.Schedule
	}{
		{name: "same time", task: &domain.Schedule{Name: "daily", Cron: "0 4 * * *", TZ: "UTC"}},
		{name: "different timezone", task: &domain.Schedule{Name: "daily", Cron: "0 13 * * *", TZ: "Asia/Tokyo"}},
		{name: "removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schedules := []domain.Schedule{{Name: "consolidate", Cron: "0 4 * * *", TZ: "UTC"}}
			if tc.task != nil {
				schedules = append(schedules, *tc.task)
			}
			last, _, err := lastTaskFire(schedules, "daily", fire)
			require.NoError(t, err)
			require.True(t, last)
		})
	}
}
