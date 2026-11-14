package reconcile

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func TestFailedScheduleFireRetriesThenNotifiesOnce(t *testing.T) {
	for _, name := range []string{"daily", "heartbeat", "consolidate"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				notes := make(chan string, 4)
				f := newDeliveryRig(t, Config{IdleTimeout: 5 * time.Minute, Procedure: restProcedure,
					NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil },
					Completed:   func(context.Context, string, time.Time) error { t.Error("failed turn completed"); return nil },
				})
				f.model.idle, f.model.manualAck = true, true
				require.NoError(t, repository.NewScheduleRepository(f.db).Save(t.Context(), domain.Schedule{
					Name: name, Cron: "0 0 * * *", TZ: "UTC", Prompt: "scheduled work", NextRun: time.Now(),
				}))
				stop := f.start(t)
				s := take(t, f.model.opened)
				key := s.config.Key
				require.Equal(t, domain.SessionKey("task:"+name), key)
				var id int64
				var text string
				for attempt := 1; attempt <= 3; attempt++ {
					input := take(t, s.inputs)
					if attempt == 1 {
						id, text = input.ID, input.Text
					} else {
						require.Equal(t, id, input.ID)
						require.Equal(t, fmt.Sprintf("An earlier attempt at this input failed: outage %d. Check what it already did before you repeat a step.\n\n%s", attempt-1, text), input.Text)
						require.Equal(t, key, s.config.Key)
						require.Equal(t, "resume-token", s.config.ResumeToken)
					}
					s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
					f.status(t, id, "done")
					at := time.Now()
					cause := fmt.Sprintf("outage %d", attempt)
					s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New(cause)}
					s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
					f.turn(t, string(key), false)
					s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
					synctest.Wait()
					var status, failure string
					var attempts int
					var retry time.Time
					require.NoError(t, f.db.QueryRow("SELECT status,attempts,retry_at,last_error FROM inbox WHERE id=?", id).Scan(&status, &attempts, &retry, &failure))
					require.Equal(t, attempt, attempts)
					require.Equal(t, cause, failure)
					require.Equal(t, "resume-token", f.turn(t, string(key), false).ResumeToken)
					if attempt == 3 {
						require.Equal(t, "dead", status)
						break
					}
					require.Equal(t, "pending", status)
					delay := 15 * time.Minute
					if attempt == 2 {
						delay = 30 * time.Minute
					}
					require.True(t, retry.Equal(at.Add(delay)))
					require.Empty(t, notes)
					time.Sleep(5 * time.Minute)
					take(t, s.closed)
					time.Sleep(time.Until(retry) - time.Nanosecond)
					synctest.Wait()
					require.Empty(t, f.model.opened)
					time.Sleep(time.Nanosecond)
					s = take(t, f.model.opened)
				}
				require.Contains(t, take(t, notes), "failed after 3 attempts: outage 3")
				stop()
				stop = f.start(t)
				synctest.Wait()
				active, err := f.inbox.Active(t.Context())
				require.NoError(t, err)
				require.Empty(t, active)
				require.Empty(t, f.model.opened)
				require.Empty(t, notes)
				var notices int
				require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'failure:%'").Scan(&notices))
				require.Equal(t, 1, notices)
				stop()
			})
		})
	}
}

func TestFailedTaskFollowupsDoNotRetry(t *testing.T) {
	for _, source := range []string{"watch:1", "wake:1", "rest:1", "task:1"} {
		t.Run(source, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryRig(t, Config{Procedure: restProcedure})
				f.model.idle, f.model.manualAck = true, true
				item := TaskItem("daily", "follow-up", "")
				item.SourceKey = source
				if source == "rest:1" {
					item.Payload.Procedure = "rest"
					require.NoError(t, f.sessions.Save(t.Context(), domain.Session{Key: item.SessionKey, ResumeToken: "resume"}))
				}
				id, err := f.inbox.Enqueue(t.Context(), item)
				require.NoError(t, err)
				f.start(t)
				s := take(t, f.model.opened)
				take(t, s.inputs)
				s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
				f.status(t, id, "done")
				s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("failed")}
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				f.turn(t, string(item.SessionKey), false)
				synctest.Wait()
				f.status(t, id, "done")
				var attempts int
				require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
				require.Zero(t, attempts)
				active, err := f.inbox.Active(t.Context())
				require.NoError(t, err)
				require.Empty(t, active)
				require.Empty(t, s.inputs)
			})
		})
	}
}

func TestFailedFireRefusalKeepsDeliveryBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{RetryDelay: 10 * time.Second})
		f.model.idle, f.model.manualAck = true, true
		item := TaskItem("daily", "work", "")
		item.SourceKey = "schedule:daily:" + time.Now().Format(time.RFC3339Nano)
		id, err := f.inbox.Enqueue(t.Context(), item)
		require.NoError(t, err)
		f.start(t)
		s := take(t, f.model.opened)
		require.Equal(t, id, take(t, s.inputs).ID)
		refusal := domain.ModelEvent{Kind: domain.ModelEventInputRefused, InputID: id, Err: errors.New("refused")}
		at := time.Now()
		s.events <- refusal
		s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("turn error")}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		synctest.Wait()
		var status, failure string
		var attempts int
		var retry time.Time
		require.NoError(t, f.db.QueryRow("SELECT status,attempts,retry_at,last_error FROM inbox WHERE id=?", id).Scan(&status, &attempts, &retry, &failure))
		require.Equal(t, "pending", status)
		require.Equal(t, 1, attempts)
		require.Equal(t, "refused", failure)
		require.True(t, retry.Equal(at.Add(10*time.Second)))
		time.Sleep(time.Until(retry))
		require.Equal(t, id, take(t, s.inputs).ID)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
		f.status(t, id, "done")
		at = time.Now()
		s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("turn error")}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		f.turn(t, string(item.SessionKey), false)
		synctest.Wait()
		require.NoError(t, f.db.QueryRow("SELECT status,attempts,retry_at,last_error FROM inbox WHERE id=?", id).Scan(&status, &attempts, &retry, &failure))
		require.Equal(t, "pending", status)
		require.Equal(t, 2, attempts)
		require.Equal(t, "turn error", failure)
		require.True(t, retry.Equal(at.Add(30*time.Minute)))
	})
}

func TestPendingFireRetryHoldsTaskRest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{Procedure: restProcedure})
		f.model.idle = true
		installTaskRestSchedules(t, f, "0 0 * * *")
		f.start(t)
		s := take(t, f.model.opened)
		input := take(t, s.inputs)
		f.status(t, input.ID, "done")
		at := time.Now()
		s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("outage")}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		f.turn(t, "task:daily", false)
		synctest.Wait()
		f.status(t, input.ID, "pending")
		var rests int
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'rest:%'").Scan(&rests))
		require.Zero(t, rests)
		require.Empty(t, s.inputs)
		time.Sleep(time.Until(at.Add(15*time.Minute)) - time.Nanosecond)
		synctest.Wait()
		require.Empty(t, s.inputs)
		require.Empty(t, s.closed)
		require.Equal(t, "resume-token", f.turn(t, "task:daily", false).ResumeToken)
		time.Sleep(time.Nanosecond)
		require.Equal(t, input.ID, take(t, s.inputs).ID)
		f.status(t, input.ID, "done")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		finishTaskRest(t, f, s)
	})
}
