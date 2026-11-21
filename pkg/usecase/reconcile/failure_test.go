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

func TestFailedScheduleTurnNotifiesWithoutRetry(t *testing.T) {
	for _, name := range []string{"daily", "heartbeat", "rest"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				notes := make(chan string, 4)
				f := newDeliveryRig(t, Config{Procedure: testProcedure,
					NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil },
				})
				f.model.idle = true
				require.NoError(t, repository.NewScheduleRepository(f.db).Save(t.Context(), domain.Schedule{
					Name: name, Cron: "0 0 * * *", TZ: "UTC", Prompt: "scheduled work", NextRun: time.Now(),
				}))
				f.start(t)
				s := take(t, f.model.opened)
				input := take(t, s.inputs)
				f.status(t, input.ID, domain.InboxDone)
				s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("outage")}
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				require.Equal(t, "Turn in main failed: outage", take(t, notes))
				f.turn(t, false)
				time.Sleep(45 * time.Minute)
				synctest.Wait()
				f.status(t, input.ID, domain.InboxDone)
				var attempts int
				require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", input.ID).Scan(&attempts))
				require.Zero(t, attempts)
				require.Empty(t, s.inputs)
				require.Empty(t, f.model.opened)
				require.Empty(t, notes)
			})
		})
	}
}

func TestFailedTurnNotifiesOnceForSeveralInputs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notes := make(chan string, 4)
		f := newDeliveryRig(t, Config{NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil }})
		for _, source := range []string{"chat:1", "schedule:daily:1", "watch:1", "wake:1"} {
			item := TaskItem("daily", source, "")
			item.SourceKey = source
			_, err := f.inbox.Enqueue(t.Context(), item)
			require.NoError(t, err)
		}
		stop := f.start(t)
		s := take(t, f.model.opened)
		for range 4 {
			f.status(t, take(t, s.inputs).ID, domain.InboxDone)
		}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("turn failed")}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("turn failed")}
		synctest.Wait()
		require.Empty(t, notes)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		require.Equal(t, "Turn in main failed: turn failed", take(t, notes))
		f.turn(t, false)
		synctest.Wait()
		require.Empty(t, notes)
		var done int
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE status='done' AND attempts=0 AND last_error='' AND source_key NOT LIKE 'turn-failure:%'").Scan(&done))
		require.Equal(t, 4, done)

		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnStarted}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("background turn failed")}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		require.Equal(t, "Turn in main failed: background turn failed", take(t, notes))
		f.turn(t, false)
		next := f.enqueue(t, "room", "new input")
		require.Equal(t, "new input", take(t, s.inputs).Text)
		f.status(t, next, domain.InboxDone)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		f.turn(t, false)
		synctest.Wait()
		require.Empty(t, s.closed)
		stop()
		f.start(t)
		synctest.Wait()
		active, err := f.inbox.Active(t.Context())
		require.NoError(t, err)
		require.Empty(t, active)
		require.Empty(t, f.model.opened)
		require.Empty(t, notes)
		var notices int
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'turn-failure:%'").Scan(&notices))
		require.Equal(t, 2, notices)
	})
}

func TestPreparationFailureEndsOnlyItsRow(t *testing.T) {
	for _, phase := range []string{"Procedure", "Prepare"} {
		for _, running := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/running=%t", phase, running), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					notes := make(chan string, 4)
					cfg := Config{NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil }}
					if phase == "Procedure" {
						cfg.Procedure = func(context.Context, string) (string, error) { return "", errors.New("cannot read procedure") }
					} else {
						cfg.Prepare = func(_ context.Context, item domain.InboxItem) (domain.UserInput, error) {
							if item.Payload.Input.Text == "bad" {
								return domain.UserInput{}, errors.New("cannot prepare input")
							}
							return item.Payload.Input, nil
						}
					}
					f := newDeliveryRig(t, cfg)
					f.start(t)
					var s *sessionFake
					if running {
						id := f.enqueue(t, "room", "first")
						s = take(t, f.model.opened)
						take(t, s.inputs)
						f.status(t, id, domain.InboxDone)
					}
					item := TaskItem("broken", "bad", "")
					if phase == "Procedure" {
						item.Payload.Procedure = "broken"
					}
					id, err := f.inbox.Enqueue(t.Context(), item)
					require.NoError(t, err)
					f.runner.Notify()
					f.status(t, id, domain.InboxDead)
					require.Contains(t, take(t, notes), "failed after 1 attempts: cannot")
					if running {
						require.Equal(t, "resume-token", f.turn(t, true).ResumeToken)
					}
					next := f.enqueue(t, "room", "next")
					if !running {
						s = take(t, f.model.opened)
					}
					require.Equal(t, "next", take(t, s.inputs).Text)
					f.status(t, next, domain.InboxDone)
					synctest.Wait()
					require.Empty(t, s.closed)
					require.Empty(t, f.model.opened)
					var attempts int
					require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
					require.Equal(t, 1, attempts)
				})
			})
		}
	}
}

func TestInterruptedPreparationRecoversAfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{}, 1)
		notes := make(chan string, 4)
		f := newDeliveryRig(t, Config{
			Prepare: func(ctx context.Context, item domain.InboxItem) (domain.UserInput, error) {
				if item.Payload.Input.Text == "blocked" {
					entered <- struct{}{}
					<-ctx.Done()
					return domain.UserInput{}, ctx.Err()
				}
				return item.Payload.Input, nil
			},
			NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil },
		})
		stop := f.start(t)
		first := f.enqueue(t, "room", "first")
		s := take(t, f.model.opened)
		take(t, s.inputs)
		f.status(t, first, domain.InboxDone)
		blocked := f.enqueue(t, "room", "blocked")
		take(t, entered)
		stop()
		f.status(t, blocked, domain.InboxPending)
		var attempts int
		require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", blocked).Scan(&attempts))
		require.Zero(t, attempts)
		f.config.Prepare = nil
		f.start(t)
		s = take(t, f.model.opened)
		require.Equal(t, "resume-token", s.config.ResumeToken)
		require.Equal(t, blocked, take(t, s.inputs).ID)
		continuation := take(t, s.inputs)
		require.Contains(t, continuation.Text, "previous turn was interrupted: shutdown")
		f.status(t, blocked, domain.InboxDone)
		f.status(t, continuation.ID, domain.InboxDone)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		f.turn(t, false)
		synctest.Wait()
		require.Empty(t, notes)
	})
}
