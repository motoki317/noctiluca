package reconcile

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/stretchr/testify/require"
)

func TestPreparationHasBoundBeforeModelOpens(t *testing.T) {
	for _, procedure := range []bool{false, true} {
		t.Run(map[bool]string{false: "Prepare", true: "Procedure"}[procedure], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				notes := make(chan string, 4)
				started := make(chan time.Time, 1)
				block := func(ctx context.Context) error {
					deadline, ok := ctx.Deadline()
					started <- deadline
					if !ok {
						return errors.New("preparation context has no deadline")
					}
					<-ctx.Done()
					return ctx.Err()
				}
				f := newDeliveryRig(t, Config{
					NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil },
					Procedure: func(ctx context.Context, _ string) (string, error) {
						return "", block(ctx)
					},
					Prepare: func(ctx context.Context, item domain.InboxItem) (domain.UserInput, error) {
						if item.Payload.Input.Text == "blocked" {
							return domain.UserInput{}, block(ctx)
						}
						return item.Payload.Input, nil
					},
				})
				f.model.send = func(ctx context.Context, _ domain.UserInput) error {
					if _, ok := ctx.Deadline(); ok {
						return errors.New("delivery context still has a deadline")
					}
					return nil
				}
				f.start(t)
				item := TaskItem("blocked", "blocked", "")
				if procedure {
					item.Payload.Procedure = "blocked"
				}
				at := time.Now()
				id, err := f.inbox.Enqueue(t.Context(), item)
				require.NoError(t, err)
				f.runner.Notify()
				require.WithinDuration(t, at.Add(operationTimeout), take(t, started), 0)
				require.Empty(t, f.model.opened)
				time.Sleep(operationTimeout)
				f.status(t, id, "dead")
				require.Contains(t, take(t, notes), "failed after 1 attempts: context deadline exceeded")
				require.Empty(t, f.model.opened)
				next := f.enqueue(t, "room", "next")
				take(t, f.model.opened)
				f.status(t, next, "done")
			})
		})
	}
}

func TestPromptHasBoundBeforeModelOpens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan time.Time, 1)
		notes := make(chan string, 2)
		var calls atomic.Int32
		f := newDeliveryRig(t, Config{MaxAttempts: 1,
			NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil },
			Prompt: func(ctx context.Context, _ domain.ToolSession) (string, error) {
				if calls.Add(1) == 1 {
					deadline, ok := ctx.Deadline()
					started <- deadline
					if !ok {
						return "", errors.New("prompt context has no deadline")
					}
					<-ctx.Done()
					return "", ctx.Err()
				}
				return "tool notes", nil
			},
		})
		f.model.send = func(ctx context.Context, _ domain.UserInput) error {
			if _, ok := ctx.Deadline(); ok {
				return errors.New("delivery context still has a deadline")
			}
			return nil
		}
		f.start(t)
		at := time.Now()
		id := f.enqueue(t, "room", "blocked")
		require.WithinDuration(t, at.Add(operationTimeout), take(t, started), 0)
		require.Empty(t, f.model.opened)
		time.Sleep(operationTimeout)
		f.status(t, id, "dead")
		require.Contains(t, take(t, notes), "context deadline exceeded")
		require.Empty(t, f.model.opened)
		next := f.enqueue(t, "room", "next")
		s := take(t, f.model.opened)
		require.Contains(t, s.config.SystemPrompt, "tool notes")
		f.status(t, next, "done")
	})
}

func TestOwnerNoticeTimeoutKeepsRetryBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan time.Time, 2)
		notes := make(chan string, 2)
		f := newDeliveryRig(t, Config{RetryDelay: 100 * time.Millisecond, MaxAttempts: 2,
			NotifyOwner: func(ctx context.Context, text string) error {
				if text != "blocked" {
					notes <- text
					return nil
				}
				deadline, ok := ctx.Deadline()
				started <- deadline
				if !ok {
					return errors.New("notice context has no deadline")
				}
				<-ctx.Done()
				return ctx.Err()
			},
		})
		f.start(t)
		item := domain.NewOwnerNotice("blocked", "blocked")
		id, err := f.inbox.Enqueue(t.Context(), item)
		require.NoError(t, err)
		f.runner.Notify()
		at := time.Now()
		for attempt := 1; attempt <= 2; attempt++ {
			require.WithinDuration(t, at.Add(operationTimeout), take(t, started), 0)
			time.Sleep(operationTimeout)
			f.status(t, id, map[bool]string{false: "pending", true: "dead"}[attempt == 2])
			if attempt == 1 {
				require.NoError(t, f.db.QueryRow("SELECT retry_at FROM inbox WHERE id=?", id).Scan(&at))
				time.Sleep(time.Until(at))
			}
		}
		var attempts int
		require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
		require.Equal(t, 2, attempts)
		next := domain.NewOwnerNotice("next", "next")
		next.SessionKey = item.SessionKey
		nextID, err := f.inbox.Enqueue(t.Context(), next)
		require.NoError(t, err)
		f.runner.Notify()
		require.Equal(t, "next", take(t, notes))
		f.status(t, nextID, "done")
		synctest.Wait()
		active, err := f.inbox.Active(t.Context())
		require.NoError(t, err)
		require.Empty(t, active)
		require.Empty(t, f.model.opened)
		require.Empty(t, notes)
	})
}
