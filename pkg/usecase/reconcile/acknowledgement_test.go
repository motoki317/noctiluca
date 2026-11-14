package reconcile

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func TestInputsAwaitTheirOwnStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Prepare: func(_ context.Context, item domain.InboxItem) (domain.UserInput, error) {
			return domain.UserInput{ID: -1, Text: item.Payload.Input.Text}, nil
		}})
		f.model.manualAck = true
		f.start(t)
		first := f.enqueue(t, "room", "first")
		s := take(t, f.model.opened)
		require.Equal(t, first, take(t, s.inputs).ID)
		f.status(t, first, "dispatched")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: first}
		f.status(t, first, "done")
		deadline := f.turn(t, "room", true).Turn.Deadline
		require.Equal(t, first, f.turn(t, "room", true).Turn.InputID)
		second := f.enqueue(t, "room", "second")
		third := f.enqueue(t, "room", "third")
		require.Equal(t, second, take(t, s.inputs).ID)
		require.Equal(t, third, take(t, s.inputs).ID)
		synctest.Wait()
		f.status(t, second, "dispatched")
		f.status(t, third, "dispatched")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: third}
		f.status(t, third, "done")
		f.status(t, second, "dispatched")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: second}
		f.status(t, second, "done")
		require.Equal(t, deadline, f.turn(t, "room", true).Turn.Deadline)
		require.Equal(t, first, f.turn(t, "room", true).Turn.InputID)
		require.Empty(t, s.inputs)
		require.Empty(t, f.model.opened)
	})
}

func TestUnstartedInputsRedeliverInOrder(t *testing.T) {
	for _, reason := range []string{"deadline", "exit", "recovery"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryRig(t, Config{TurnTimeout: time.Second})
				f.model.manualAck = true
				stop := f.start(t)
				first := f.enqueue(t, "room", "first")
				s := take(t, f.model.opened)
				require.Equal(t, first, take(t, s.inputs).ID)
				s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: first}
				f.status(t, first, "done")
				time.Sleep(100 * time.Millisecond)
				second := f.enqueue(t, "room", "second")
				third := f.enqueue(t, "room", "third")
				require.Equal(t, second, take(t, s.inputs).ID)
				require.Equal(t, third, take(t, s.inputs).ID)
				synctest.Wait()
				f.status(t, second, "dispatched")
				f.status(t, third, "dispatched")
				switch reason {
				case "exit":
					close(s.events)
				case "recovery":
					stop()
					for _, id := range []int64{second, third} {
						claimed, err := f.inbox.Claim(t.Context(), id, time.Now().Add(time.Hour))
						require.NoError(t, err)
						require.True(t, claimed)
					}
					f.start(t)
				}
				take(t, s.closed)
				s = take(t, f.model.opened)
				require.Equal(t, "resume-token", s.config.ResumeToken)
				require.Equal(t, second, take(t, s.inputs).ID)
				require.Equal(t, third, take(t, s.inputs).ID)
				s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: second}
				s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: third}
				for _, id := range []int64{first, second, third} {
					f.status(t, id, "done")
					var attempts int
					require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
					require.Zero(t, attempts)
				}
				if reason != "deadline" {
					note := take(t, s.inputs)
					require.Greater(t, note.ID, third)
					require.Contains(t, note.Text, "previous turn was interrupted")
					s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: note.ID}
					f.status(t, note.ID, "done")
				}
				require.Empty(t, s.inputs)
			})
		})
	}
}

func TestRefusedInputRetriesWithBackoff(t *testing.T) {
	for _, lane := range []string{"conv", "task"} {
		t.Run(lane, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				notes := make(chan string, 2)
				f := newDeliveryRig(t, Config{TurnTimeout: time.Hour, RetryDelay: 100 * time.Millisecond, MaxAttempts: 3,
					NotifyOwner: func(_ context.Context, note string) error { notes <- note; return nil },
					Completed:   func(context.Context, string, time.Time) error { t.Error("refused input completed a turn"); return nil },
					Prepare: func(_ context.Context, item domain.InboxItem) (domain.UserInput, error) {
						return domain.UserInput{Text: "prepared " + item.Payload.Input.Text}, nil
					},
				})
				f.model.manualAck = true
				f.start(t)
				item := TaskItem("refused", "refused", "")
				item.Lane = lane
				id, err := f.inbox.Enqueue(t.Context(), item)
				require.NoError(t, err)
				f.runner.Notify()
				s := take(t, f.model.opened)
				var retryAt time.Time
				for attempt := 1; attempt <= 3; attempt++ {
					input := take(t, s.inputs)
					require.Equal(t, id, input.ID)
					if attempt == 1 {
						require.Equal(t, "prepared refused", input.Text)
					} else {
						require.Equal(t, "An earlier attempt at this input failed: refused. Check what it already did before you repeat a step.\n\nprepared refused", input.Text)
					}
					at := time.Now()
					if attempt > 1 {
						require.True(t, retryAt.Equal(at))
					}
					s.events <- domain.ModelEvent{Kind: domain.ModelEventInputRefused, InputID: id, Err: errors.New("refused")}
					if lane == "conv" {
						s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
					}
					synctest.Wait()
					var status, failure string
					var attempts int
					require.NoError(t, f.db.QueryRow("SELECT status,attempts,retry_at,last_error FROM inbox WHERE id=?", id).Scan(&status, &attempts, &retryAt, &failure))
					require.Equal(t, attempt, attempts)
					require.Equal(t, "refused", failure)
					require.True(t, retryAt.Equal(at.Add(f.config.RetryDelay*time.Duration(1<<(attempt-1)))))
					if attempt < 3 {
						require.Equal(t, "pending", status)
					} else {
						require.Equal(t, "dead", status)
					}
					require.Empty(t, s.inputs)
				}
				require.Contains(t, take(t, notes), "failed after 3 attempts: refused")
				synctest.Wait()
				var notices int
				require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'failure:%'").Scan(&notices))
				require.Equal(t, 1, notices)
				require.Empty(t, notes)
			})
		})
	}
}

func TestRefusedOpeningInputAllowsIdleClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{IdleTimeout: 100 * time.Millisecond, TurnTimeout: time.Hour, RetryDelay: time.Hour})
		f.model.manualAck = true
		f.model.idle = true
		f.start(t)
		id := f.enqueue(t, "room", "refused")
		s := take(t, f.model.opened)
		require.Equal(t, id, take(t, s.inputs).ID)
		f.turn(t, "room", true)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputRefused, InputID: id, Err: errors.New("refused")}
		f.status(t, id, "pending")
		f.turn(t, "room", false)
		take(t, s.closed)
		synctest.Wait()
		var attempts, notes int
		require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
		require.Equal(t, 1, attempts)
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM model_events").Scan(&notes))
		require.Zero(t, notes)
		require.Empty(t, f.model.opened)
	})
}

func TestRefusalPreservesOtherInputsTurn(t *testing.T) {
	for _, openingStarted := range []bool{false, true} {
		t.Run(map[bool]string{false: "another-unstarted", true: "another-started"}[openingStarted], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryRig(t, Config{RetryDelay: time.Hour})
				f.model.manualAck = true
				f.start(t)
				first := f.enqueue(t, "room", "schedule:daily:"+time.Now().Format(time.RFC3339Nano))
				s := take(t, f.model.opened)
				require.Equal(t, first, take(t, s.inputs).ID)
				if openingStarted {
					s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: first}
					f.status(t, first, "done")
				}
				second := f.enqueue(t, "room", "second")
				require.Equal(t, second, take(t, s.inputs).ID)
				refused := first
				if openingStarted {
					refused = second
				}
				s.events <- domain.ModelEvent{Kind: domain.ModelEventInputRefused, InputID: refused, Err: errors.New("refused")}
				f.status(t, refused, "pending")
				synctest.Wait()
				turn := f.turn(t, "room", true).Turn
				require.Equal(t, first, turn.InputID)
				require.True(t, turn.Failed)
				if !openingStarted {
					s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: second}
					f.status(t, second, "done")
				}
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				f.turn(t, "room", false)
				if openingStarted {
					f.status(t, first, "done")
				}
			})
		})
	}
}

func TestUnstartedTaskRedeliversWithoutContinuation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{})
		f.model.manualAck = true
		f.start(t)
		item := TaskItem("unstarted", "original task", "")
		id, err := f.inbox.Enqueue(t.Context(), item)
		require.NoError(t, err)
		f.runner.Notify()
		s := take(t, f.model.opened)
		require.Equal(t, id, take(t, s.inputs).ID)
		require.Equal(t, id, f.turn(t, string(item.SessionKey), true).Turn.InputID)
		f.status(t, id, "dispatched")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnStarted}
		synctest.Wait()
		events := repository.NewEventRepository(f.db)
		before, err := events.InputContext(t.Context(), item.SessionKey, "", time.Now())
		require.NoError(t, err)
		require.True(t, before.Running)
		close(s.events)
		take(t, s.closed)
		s = take(t, f.model.opened)
		require.Equal(t, id, take(t, s.inputs).ID)
		after, err := events.InputContext(t.Context(), item.SessionKey, "", time.Now())
		require.NoError(t, err)
		require.False(t, after.Running)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		f.status(t, id, "done")
		f.turn(t, string(item.SessionKey), false)
		synctest.Wait()
		var rows, notes, attempts int
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox").Scan(&rows))
		require.Equal(t, 1, rows)
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM model_events WHERE kind='error'").Scan(&notes))
		require.Equal(t, 1, notes)
		require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
		require.Zero(t, attempts)
		require.Empty(t, s.inputs)
	})
}

func TestBufferedStartSurvivesDeliveryDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{TurnTimeout: time.Second, RetryDelay: time.Hour})
		f.model.manualAck = true
		sent := make(chan domain.UserInput, 2)
		f.model.send = func(ctx context.Context, input domain.UserInput) error {
			sent <- input
			if input.Text == "blocked" {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}
		stop := f.start(t)
		first := f.enqueue(t, "room", "started")
		s := take(t, f.model.opened)
		require.Equal(t, first, take(t, sent).ID)
		time.Sleep(100 * time.Millisecond)
		second := f.enqueue(t, "room", "blocked")
		require.Equal(t, second, take(t, sent).ID)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: first}
		take(t, s.closed)
		stop()
		f.status(t, first, "done")
		f.status(t, second, "pending")
		for _, id := range []int64{first, second} {
			var attempts int
			require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
			require.Zero(t, attempts)
		}
	})
}

func TestLateSuccessfulSendWithoutStartFailsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{TaskTimeout: time.Second, RetryDelay: time.Hour, NotifyOwner: func(context.Context, string) error { return nil }})
		f.model.manualAck = true
		f.model.send = func(ctx context.Context, _ domain.UserInput) error {
			<-ctx.Done()
			return nil
		}
		stop := f.start(t)
		id, err := f.inbox.Enqueue(t.Context(), TaskItem("late", "work", ""))
		require.NoError(t, err)
		f.runner.Notify()
		s := take(t, f.model.opened)
		take(t, s.closed)
		synctest.Wait()
		stop()
		f.status(t, id, "pending")
		var attempts int
		require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
		require.Equal(t, 1, attempts)
		var rows, notes int
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox").Scan(&rows))
		require.Equal(t, 1, rows)
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM model_events").Scan(&notes))
		require.Equal(t, 1, notes)
	})
}

func TestLateInputStartDoesNotRecreateTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{})
		f.model.manualAck = true
		f.start(t)
		id := f.enqueue(t, "room", "first")
		s := take(t, f.model.opened)
		require.Equal(t, id, take(t, s.inputs).ID)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		f.turn(t, "room", false)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
		f.status(t, id, "done")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputRefused, InputID: id, Err: errors.New("duplicate")}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: -1}
		synctest.Wait()
		saved, err := f.sessions.Get(t.Context(), "room")
		require.NoError(t, err)
		require.Nil(t, saved.Turn)
		var attempts int
		require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
		require.Zero(t, attempts)
	})
}

func TestUnstartedInputDeadlineFailsOnlyExpiredItem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{TurnTimeout: time.Second, RetryDelay: time.Hour})
		f.model.manualAck = true
		sent := make(chan domain.UserInput, 2)
		f.model.send = func(ctx context.Context, input domain.UserInput) error {
			sent <- input
			if input.Text == "later" {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}
		stop := f.start(t)
		first := f.enqueue(t, "room", "silent")
		s := take(t, f.model.opened)
		require.Equal(t, first, take(t, sent).ID)
		time.Sleep(100 * time.Millisecond)
		second := f.enqueue(t, "room", "later")
		require.Equal(t, second, take(t, sent).ID)
		take(t, s.closed)
		synctest.Wait()
		stop()
		f.status(t, first, "pending")
		var attempts int
		var failure string
		require.NoError(t, f.db.QueryRow("SELECT attempts,last_error FROM inbox WHERE id=?", first).Scan(&attempts, &failure))
		require.Equal(t, 1, attempts)
		require.Equal(t, context.DeadlineExceeded.Error(), failure)
		f.status(t, second, "pending")
		require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", second).Scan(&attempts))
		require.Zero(t, attempts)
		var marked int
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM model_events WHERE error LIKE '%delivery deadline exceeded%'").Scan(&marked))
		require.Equal(t, 1, marked)
		require.Empty(t, f.model.opened)
	})
}

func TestUnstartedTaskReservesLaneAndKeepsMetadata(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		completed := make(chan string, 2)
		f := newDeliveryRig(t, Config{Completed: func(_ context.Context, procedure string, _ time.Time) error { completed <- procedure; return nil }})
		f.model.manualAck, f.model.idle = true, true
		f.start(t)
		item := TaskItem("first", "work", "")
		item.Payload.Procedure = "heartbeat"
		item.Payload.Continuations = 1
		id, err := f.inbox.Enqueue(t.Context(), item)
		require.NoError(t, err)
		f.runner.Notify()
		s := take(t, f.model.opened)
		require.Equal(t, id, take(t, s.inputs).ID)
		saved := f.turn(t, string(item.SessionKey), true)
		require.Equal(t, id, saved.Turn.InputID)
		require.Equal(t, "heartbeat", saved.Turn.Procedure)
		require.Equal(t, 1, saved.Turn.Continuations)
		f.status(t, id, "dispatched")
		_, err = f.inbox.Enqueue(t.Context(), TaskItem("next", "next", ""))
		require.NoError(t, err)
		f.runner.Notify()
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		require.Equal(t, "heartbeat", take(t, completed))
		synctest.Wait()
		require.Empty(t, f.model.opened)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
		f.status(t, id, "done")
		next := take(t, f.model.opened)
		require.Equal(t, domain.SessionKey("task:next"), next.config.Key)
		input := take(t, next.inputs)
		next.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: input.ID}
		f.status(t, input.ID, "done")
	})
}
