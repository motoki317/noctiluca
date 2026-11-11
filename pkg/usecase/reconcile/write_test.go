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

type delayedSessionRepository struct {
	domain.SessionRepository
	delay   time.Duration
	err     error
	started chan time.Time
}

func (r *delayedSessionRepository) Save(ctx context.Context, session domain.Session) error {
	if session.Turn == nil {
		return r.SessionRepository.Save(ctx, session)
	}
	r.started <- time.Now()
	var ready <-chan time.Time
	if r.delay > 0 {
		ready = time.After(r.delay)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ready:
		if r.err != nil {
			return r.err
		}
		return r.SessionRepository.Save(ctx, session)
	}
}

func TestHandleWriteDeadline(t *testing.T) {
	writeErr := errors.New("write failed")
	for _, phase := range []string{"running", "shutdown"} {
		for _, tc := range []struct {
			name    string
			delay   time.Duration
			err     error
			elapsed time.Duration
			wantErr error
		}{
			{name: "slow", delay: 10 * time.Second, elapsed: 10 * time.Second},
			{name: "hung", elapsed: 30 * time.Second, wantErr: context.DeadlineExceeded},
			{name: "error", delay: time.Second, err: writeErr, elapsed: time.Second, wantErr: writeErr},
		} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newDeliveryRig(t, Config{})
					sessions := &delayedSessionRepository{SessionRepository: f.sessions, delay: tc.delay, err: tc.err, started: make(chan time.Time, 1)}
					sent := make(chan domain.UserInput, 1)
					f.model.send = func(ctx context.Context, input domain.UserInput) error {
						sent <- input
						if phase == "shutdown" {
							<-ctx.Done()
						}
						return nil
					}
					f.runner = New(f.config, f.inbox, repository.NewScheduleRepository(f.db), sessions, repository.NewEventRepository(f.db), f.model, &hubFake{closed: make(chan domain.SessionKey, 1)})
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					done := make(chan error, 1)
					go func() { done <- f.runner.Start(ctx) }()
					id := f.enqueue(t, "room", "input")
					take(t, f.model.opened)
					require.Equal(t, id, take(t, sent).ID)
					if phase == "shutdown" {
						cancel()
					}
					synctest.Wait()
					started := take(t, sessions.started)
					time.Sleep(tc.elapsed - time.Nanosecond)
					synctest.Wait()
					require.Empty(t, done)
					time.Sleep(time.Nanosecond)
					synctest.Wait()
					require.Equal(t, tc.elapsed, time.Since(started))
					if tc.wantErr != nil {
						require.NotEmpty(t, done)
						require.ErrorIs(t, <-done, tc.wantErr)
						return
					}
					if phase == "running" {
						require.Empty(t, done)
					}
					f.status(t, id, "done")
					cancel()
					synctest.Wait()
					require.NotEmpty(t, done)
					require.NoError(t, <-done)
				})
			})
		}
	}
}
