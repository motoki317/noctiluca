package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

type deliveryRig struct {
	db       *sql.DB
	inbox    domain.InboxRepository
	sessions domain.SessionRepository
	model    *modelFake
	hub      *hubFake
	config   Config
	runner   *Reconciler
	cancel   context.CancelFunc
}

func newDeliveryRig(t *testing.T, config Config) *deliveryRig {
	t.Helper()
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return &deliveryRig{db: db, inbox: repository.NewInboxRepository(db), sessions: repository.NewSessionRepository(db), model: &modelFake{opened: make(chan *sessionFake, 20)}, config: config}
}
func (f *deliveryRig) start(t *testing.T) func() {
	t.Helper()
	f.hub = &hubFake{closed: make(chan domain.SessionKey, 100)}
	f.runner = New(f.config, f.inbox, repository.NewScheduleRepository(f.db), f.sessions, repository.NewEventRepository(f.db), f.model, f.hub)
	ctx, cancel := context.WithCancel(t.Context())
	f.cancel = cancel
	done := make(chan error, 1)
	go func() { done <- f.runner.Start(ctx) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); require.NoError(t, take(t, done)) }) }
	t.Cleanup(stop)
	return stop
}
func (f *deliveryRig) enqueue(t *testing.T, key, text string) int64 {
	t.Helper()
	item := domain.InboxItem{SourceKey: text, SessionKey: domain.SessionKey(key), Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: text}, Context: domain.SessionContext{Conversation: domain.ConversationRef(key)}}}
	id, err := f.inbox.Enqueue(t.Context(), item)
	require.NoError(t, err)
	f.runner.Notify()
	return id
}
func (f *deliveryRig) status(t *testing.T, id int64, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var status string
		return f.db.QueryRow("SELECT status FROM inbox WHERE id=?", id).Scan(&status) == nil && status == want
	}, time.Second, time.Millisecond)
}
func (f *deliveryRig) turn(t *testing.T, key string, busy bool) domain.Session {
	t.Helper()
	var saved domain.Session
	require.Eventually(t, func() bool {
		var err error
		saved, err = f.sessions.Get(t.Context(), domain.SessionKey(key))
		return err == nil && (saved.Turn != nil) == busy
	}, time.Second, time.Millisecond)
	return saved
}
func TestConversationSteersBeforeTurnFinish(t *testing.T) {
	f := newDeliveryRig(t, Config{TurnTimeout: time.Second})
	f.start(t)
	first := f.enqueue(t, "room", "first")
	s := take(t, f.model.opened)
	require.Equal(t, "first", take(t, s.inputs).Text)
	f.status(t, first, "done")
	deadline := f.turn(t, "room", true).Turn.Deadline
	second := f.enqueue(t, "room", "steer")
	require.Equal(t, "steer", take(t, s.inputs).Text)
	f.status(t, second, "done")
	require.Equal(t, deadline, f.turn(t, "room", true).Turn.Deadline)
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.turn(t, "room", false)
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnStarted}
	require.True(t, f.turn(t, "room", true).Turn.Deadline.After(deadline))
}
func TestDeliveredConversationNeverReplays(t *testing.T) {
	for _, terminal := range []string{"failed-result", "exit", "deadline", "restart"} {
		t.Run(terminal, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := Config{TurnTimeout: time.Hour}
				if terminal == "deadline" {
					cfg.TurnTimeout = 100 * time.Millisecond
				}
				f := newDeliveryRig(t, cfg)
				stop := f.start(t)
				id := f.enqueue(t, "room", "original")
				s := take(t, f.model.opened)
				take(t, s.inputs)
				f.status(t, id, "done")
				switch terminal {
				case "failed-result":
					s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("failed")}
					s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
					f.turn(t, "room", false)
				case "exit":
					close(s.events)
					take(t, s.closed)
				case "deadline":
					take(t, s.closed)
				case "restart":
					stop()
					f.start(t)
				}
				next := f.enqueue(t, "room", "new input")
				if terminal != "failed-result" {
					s = take(t, f.model.opened)
				}
				require.Equal(t, "new input", take(t, s.inputs).Text)
				f.status(t, next, "done")
				var attempts, notes int
				require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
				require.Zero(t, attempts)
				require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'failure:%' OR source_key LIKE 'interrupted:%'").Scan(&notes))
				require.Zero(t, notes)
			})
		})
	}
}
func TestClosingProcessReservesSessionKey(t *testing.T) {
	f := newDeliveryRig(t, Config{})
	release := make(chan struct{})
	f.model.closeWait = release
	stop := f.start(t)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer func() { unblock(); stop() }()
	id := f.enqueue(t, "room", "first")
	s := take(t, f.model.opened)
	take(t, s.inputs)
	f.status(t, id, "done")
	close(s.events)
	take(t, s.closed)
	next := f.enqueue(t, "room", "after exit")
	require.Never(t, func() bool { return len(f.model.opened) > 0 }, 30*time.Millisecond, time.Millisecond)
	unblock()
	s = take(t, f.model.opened)
	require.Equal(t, "after exit", take(t, s.inputs).Text)
	f.status(t, next, "done")
}
func TestPermanentRejectionHasNoRetryOrNotice(t *testing.T) {
	f := newDeliveryRig(t, Config{Accept: func(info domain.SessionContext) bool {
		return info.Conversation != "removed"
	}, Prepare: func(context.Context, domain.InboxItem) (domain.UserInput, error) {
		return domain.UserInput{}, domain.ErrRejected
	}})
	f.start(t)
	for _, key := range []string{"removed", "sender"} {
		id := f.enqueue(t, key, key)
		f.status(t, id, "dead")
		var attempts int
		require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
		require.Zero(t, attempts)
	}
	var count int
	require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'failure:%'").Scan(&count))
	require.Zero(t, count)
}
func TestSkippedProcedurePreservesResumeToken(t *testing.T) {
	f := newDeliveryRig(t, Config{Procedure: func(context.Context, string) (string, error) { return "", nil }})
	require.NoError(t, f.sessions.Save(t.Context(), domain.Session{Key: "task:heartbeat", ResumeToken: "saved"}))
	f.start(t)
	item := TaskItem("heartbeat", "run", "")
	item.Payload.Procedure = "heartbeat"
	id, err := f.inbox.Enqueue(t.Context(), item)
	require.NoError(t, err)
	f.runner.Notify()
	f.status(t, id, "done")
	require.Equal(t, "saved", f.turn(t, "task:heartbeat", false).ResumeToken)
	require.Empty(t, f.model.opened)
}

func TestScheduleToolRequiresFiveFields(t *testing.T) {
	r := New(Config{}, nil, nil, nil, nil, nil, nil)
	for _, expression := range []string{"@daily", "@every 1h", "CRON_TZ=UTC 0 4 * * *", "0 0 4 * * *", "TZ=UTC\t*\t*\t*\t*"} {
		raw, err := json.Marshal(map[string]string{"name": "test", "cron": expression, "tz": "UTC", "prompt": "work"})
		require.NoError(t, err)
		_, err = r.Tools()[0].Handler(t.Context(), raw)
		require.ErrorContains(t, err, "five fields")
	}
}

func TestRejectedSteerLeavesCurrentTurnRunning(t *testing.T) {
	f := newDeliveryRig(t, Config{Prepare: func(_ context.Context, item domain.InboxItem) (domain.UserInput, error) {
		if item.Payload.Input.Text == "rejected" {
			return domain.UserInput{}, domain.ErrRejected
		}
		return item.Payload.Input, nil
	}})
	f.start(t)
	first := f.enqueue(t, "room", "accepted")
	s := take(t, f.model.opened)
	take(t, s.inputs)
	f.status(t, first, "done")
	next := f.enqueue(t, "room", "rejected")
	f.status(t, next, "dead")
	f.turn(t, "room", true)
	select {
	case <-s.closed:
		t.Fatal("rejected input killed accepted turn")
	default:
	}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.turn(t, "room", false)
	third := f.enqueue(t, "room", "third accepted")
	require.Equal(t, "third accepted", take(t, s.inputs).Text)
	f.status(t, third, "done")
	require.Empty(t, f.model.opened)
}
func TestCompletionWaitsForSuccessfulTurnFinish(t *testing.T) {
	finished := make(chan time.Time, 1)
	f := newDeliveryRig(t, Config{Completed: func(_ context.Context, _ string, at time.Time) error { finished <- at; return nil }})
	f.start(t)
	id := f.enqueue(t, "room", "first")
	s := take(t, f.model.opened)
	take(t, s.inputs)
	f.status(t, id, "done")
	require.Empty(t, finished)
	s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("failed result")}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.turn(t, "room", false)
	require.Empty(t, finished)
	id = f.enqueue(t, "room", "second")
	take(t, s.inputs)
	f.status(t, id, "done")
	before := time.Now()
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	require.False(t, take(t, finished).Before(before))
}

func TestTaskDeadlineNotifiesWithoutContinuing(t *testing.T) {
	// Runner scheduling must not consume the deadline used to expire the hung task.
	synctest.Test(t, func(t *testing.T) {
		notes := make(chan string, 4)
		f := newDeliveryRig(t, Config{TaskTimeout: 50 * time.Millisecond, NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil }})
		stop := f.start(t)
		item := TaskItem("hung", "work", "")
		id, err := f.inbox.Enqueue(t.Context(), item)
		require.NoError(t, err)
		f.runner.Notify()
		s := take(t, f.model.opened)
		take(t, s.inputs)
		f.status(t, id, "done")
		take(t, s.closed)
		require.Contains(t, take(t, notes), "Task hung stopped: turn deadline exceeded")
		require.Eventually(t, func() bool {
			active, err := f.inbox.Active(t.Context())
			return err == nil && len(active) == 0
		}, time.Second, time.Millisecond)
		stop()
		stop = f.start(t)
		defer stop()
		require.Never(t, func() bool { return len(f.model.opened) > 0 || len(notes) > 0 }, 120*time.Millisecond, time.Millisecond)
		var marked int
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM model_events WHERE session_key=? AND error LIKE '%turn deadline exceeded%'", item.SessionKey).Scan(&marked))
		require.Equal(t, 1, marked)
	})
}

func TestLateTaskDeliveryIsReportedNotContinued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notes := make(chan string, 4)
		f := newDeliveryRig(t, Config{TaskTimeout: 50 * time.Millisecond, NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil }})
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		f.model.send = func(context.Context, domain.UserInput) error { <-release; return nil }
		stop := f.start(t)
		item := TaskItem("hung", "work", "")
		id, err := f.inbox.Enqueue(t.Context(), item)
		require.NoError(t, err)
		f.runner.Notify()
		s := take(t, f.model.opened)
		// Hold Send until retirement so success cannot beat the delivery deadline.
		require.Equal(t, item.SessionKey, take(t, f.hub.closed))
		unblock()
		take(t, s.closed)
		f.status(t, id, "done")
		require.Equal(t, "Task hung stopped: delivery deadline exceeded", take(t, notes))
		stop()
		f.start(t)
		synctest.Wait()
		active, err := f.inbox.Active(t.Context())
		require.NoError(t, err)
		require.Empty(t, active)
		require.Nil(t, f.turn(t, string(item.SessionKey), false).Turn)
		require.Empty(t, f.model.opened)
		require.Empty(t, notes)
		var inputs, marked, attempts int
		require.NoError(t, f.db.QueryRow("SELECT count(*),sum(attempts) FROM inbox WHERE session_key=?", item.SessionKey).Scan(&inputs, &attempts))
		require.Equal(t, 1, inputs)
		require.Zero(t, attempts)
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM model_events WHERE session_key=? AND error LIKE '%delivery deadline exceeded%'", item.SessionKey).Scan(&marked))
		require.Equal(t, 1, marked)
	})
}

func TestTaskDeliveryDeadlineBeforeRetirement(t *testing.T) {
	for _, remaining := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		t.Run(remaining.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryRig(t, Config{})
				item := TaskItem("late", "work", "")
				id, err := f.inbox.Enqueue(t.Context(), item)
				require.NoError(t, err)
				item.ID = id
				item.Deadline = time.Now().Add(remaining)
				claimed, err := f.inbox.Claim(t.Context(), id, item.Deadline)
				require.NoError(t, err)
				require.True(t, claimed)
				r := New(f.config, f.inbox, nil, f.sessions, nil, f.model, &hubFake{closed: make(chan domain.SessionKey, 1)})
				_, cancel := context.WithCancel(t.Context())
				defer cancel()
				p := &process{key: item.SessionKey, lane: item.Lane, info: item.Payload.Context, delivering: &item, cancel: cancel, model: &sessionFake{}}
				r.live[p.key] = p
				require.NoError(t, r.handle(t.Context(), result{process: p, delivered: true}))
				require.NoError(t, r.handle(t.Context(), result{process: p, event: &domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}}))
				f.status(t, id, "done")
				saved, err := f.sessions.Get(t.Context(), item.SessionKey)
				require.NoError(t, err)
				require.NotNil(t, saved.Turn)
				require.Equal(t, remaining <= 0, p.closing)
				if remaining > 0 {
					return
				}
				require.Equal(t, domain.DeliveryDeadlineExceeded, p.reason)
				require.NoError(t, r.handle(t.Context(), result{process: p, exited: true}))
				active, err := f.inbox.Active(t.Context())
				require.NoError(t, err)
				require.Len(t, active, 1)
				require.Equal(t, "Task late stopped: delivery deadline exceeded", active[0].Payload.Notice)
			})
		})
	}
}

func TestInterruptedTaskContinuesOnlyOnce(t *testing.T) {
	f := newDeliveryRig(t, Config{})
	f.start(t)
	item := TaskItem("interrupted", "work", "")
	id, err := f.inbox.Enqueue(t.Context(), item)
	require.NoError(t, err)
	f.runner.Notify()
	s := take(t, f.model.opened)
	take(t, s.inputs)
	f.status(t, id, "done")
	close(s.events)
	s = take(t, f.model.opened)
	require.Contains(t, take(t, s.inputs).Text, "previous turn was interrupted")
	require.Eventually(t, func() bool {
		saved, err := f.sessions.Get(t.Context(), item.SessionKey)
		return err == nil && saved.Turn != nil && saved.Turn.Continuations == 1
	}, time.Second, time.Millisecond)
	close(s.events)
	require.Eventually(t, func() bool {
		var n int
		return f.db.QueryRow("SELECT count(*) FROM model_events WHERE session_key=? AND kind='error'", item.SessionKey).Scan(&n) == nil && n == 2
	}, time.Second, time.Millisecond)
	active, err := f.inbox.Active(t.Context())
	require.NoError(t, err)
	require.Empty(t, active)
	require.Empty(t, f.model.opened)
}

func TestDeliveryDeadlineRetriesUnsentInputAndNotice(t *testing.T) {
	for _, notice := range []bool{false, true} {
		t.Run(map[bool]string{false: "input", true: "notice"}[notice], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := make(chan time.Time, 3)
				notes := make(chan string, 4)
				fail := func(ctx context.Context) error {
					started <- time.Now()
					<-ctx.Done()
					return ctx.Err()
				}
				cfg := Config{TurnTimeout: 40 * time.Millisecond, RetryDelay: 100 * time.Millisecond, MaxAttempts: 3}
				cfg.NotifyOwner = func(ctx context.Context, text string) error {
					if text == "owner note" {
						return fail(ctx)
					}
					notes <- text
					return nil
				}
				cfg.Prepare = func(ctx context.Context, item domain.InboxItem) (domain.UserInput, error) {
					return item.Payload.Input, fail(ctx)
				}
				f := newDeliveryRig(t, cfg)
				stop := f.start(t)
				item := domain.InboxItem{SourceKey: "unsent", SessionKey: "session", Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: "original"}}}
				if notice {
					item.Payload.Notice = "owner note"
				}
				id, err := f.inbox.Enqueue(t.Context(), item)
				require.NoError(t, err)
				f.runner.Notify()
				var retryAt time.Time
				for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
					at := take(t, started)
					if attempt > 1 {
						require.True(t, at.Equal(retryAt), "retry must wait for its backoff")
					}
					require.Equal(t, item.SessionKey, take(t, f.hub.closed))
					synctest.Wait()
					var status, failure string
					var attempts int
					require.NoError(t, f.db.QueryRow("SELECT status,attempts,retry_at,last_error FROM inbox WHERE id=?", id).Scan(&status, &attempts, &retryAt, &failure))
					require.Equal(t, attempt, attempts)
					require.Equal(t, context.DeadlineExceeded.Error(), failure)
					require.True(t, retryAt.Equal(at.Add(cfg.TurnTimeout+cfg.RetryDelay*time.Duration(1<<(attempt-1)))))
					if attempt < cfg.MaxAttempts {
						require.Equal(t, "pending", status)
					} else {
						require.Equal(t, "dead", status)
					}
					require.Empty(t, started)
				}
				if !notice {
					require.Contains(t, take(t, notes), "for session failed after 3 attempts: context deadline exceeded")
				}
				stop()
				f.start(t)
				synctest.Wait()
				active, err := f.inbox.Active(t.Context())
				require.NoError(t, err)
				require.Empty(t, active)
				require.Empty(t, started)
				require.Empty(t, f.model.opened)
				require.Empty(t, notes)
				var failures int
				require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'failure:%'").Scan(&failures))
				if notice {
					require.Zero(t, failures)
				} else {
					require.Equal(t, 1, failures)
				}
			})
		})
	}
}

func TestAlwaysLateTaskDoesNotStarveQueuedTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notes := make(chan string, 4)
		var attempts atomic.Int32
		f := newDeliveryRig(t, Config{TaskTimeout: 50 * time.Millisecond, RetryDelay: 100 * time.Millisecond, MaxAttempts: 3,
			NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil },
			Prepare: func(ctx context.Context, item domain.InboxItem) (domain.UserInput, error) {
				if item.Payload.Context.TaskName == "late" {
					attempts.Add(1)
					<-ctx.Done()
					return domain.UserInput{}, ctx.Err()
				}
				return item.Payload.Input, nil
			},
		})
		late := TaskItem("late", "work", "")
		id, err := f.inbox.Enqueue(t.Context(), late)
		require.NoError(t, err)
		next := TaskItem("next", "next task", "")
		nextID, err := f.inbox.Enqueue(t.Context(), next)
		require.NoError(t, err)
		stop := f.start(t)
		s := take(t, f.model.opened)
		require.Equal(t, next.SessionKey, s.config.Key)
		require.Equal(t, "next task", take(t, s.inputs).Text)
		require.EqualValues(t, 1, attempts.Load())
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		f.status(t, nextID, "done")
		f.status(t, id, "dead")
		require.EqualValues(t, 3, attempts.Load())
		require.Contains(t, take(t, notes), "for task:late failed after 3 attempts: context deadline exceeded")
		stop()
		f.start(t)
		synctest.Wait()
		active, err := f.inbox.Active(t.Context())
		require.NoError(t, err)
		require.Empty(t, active)
		require.Empty(t, f.model.opened)
		require.Empty(t, notes)
		var failures, inputs, notices int
		require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&failures))
		require.Equal(t, 3, failures)
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE session_key=?", late.SessionKey).Scan(&inputs))
		require.Equal(t, 1, inputs)
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'failure:%'").Scan(&notices))
		require.Equal(t, 1, notices)
	})
}

func TestDeliveryFailureUsesClosingReason(t *testing.T) {
	for _, reason := range []string{"", domain.DeliveryDeadlineExceeded, "shutdown", "session configuration changed", "conversation no longer accepted", domain.TurnDeadlineExceeded} {
		for _, cause := range []error{context.Canceled, errors.Join(errors.New("send failed"), context.DeadlineExceeded)} {
			t.Run(reason+"/"+cause.Error(), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newDeliveryRig(t, Config{RetryDelay: time.Second})
					item := domain.InboxItem{SourceKey: "unsent", SessionKey: "session", Lane: "conv", ReadyAt: time.Now()}
					id, err := f.inbox.Enqueue(t.Context(), item)
					require.NoError(t, err)
					item.ID = id
					claimed, err := f.inbox.Claim(t.Context(), id, time.Now())
					require.NoError(t, err)
					require.True(t, claimed)
					r := New(f.config, f.inbox, nil, f.sessions, nil, f.model, &hubFake{closed: make(chan domain.SessionKey, 1)})
					_, cancel := context.WithCancel(t.Context())
					defer cancel()
					p := &process{key: item.SessionKey, lane: item.Lane, delivering: &item, cancel: cancel}
					r.live[p.key] = p
					if reason != "" {
						r.retire(p, reason)
					}
					require.NoError(t, r.handle(t.Context(), result{process: p, delivered: true, err: cause}))
					require.NoError(t, r.handle(t.Context(), result{process: p, exited: true}))
					var status, failure string
					var attempts int
					var retryAt time.Time
					require.NoError(t, f.db.QueryRow("SELECT status,attempts,retry_at,last_error FROM inbox WHERE id=?", id).Scan(&status, &attempts, &retryAt, &failure))
					require.Equal(t, "pending", status)
					if reason == "" || reason == domain.DeliveryDeadlineExceeded {
						require.Equal(t, 1, attempts)
						require.True(t, retryAt.Equal(time.Now().Add(f.config.RetryDelay)))
						if reason == domain.DeliveryDeadlineExceeded || errors.Is(cause, context.DeadlineExceeded) {
							require.Equal(t, domain.DeliveryDeadlineExceeded, p.reason)
							require.Contains(t, failure, context.DeadlineExceeded.Error())
						}
					} else {
						require.Zero(t, attempts)
						require.True(t, retryAt.IsZero())
						require.Empty(t, failure)
						require.Equal(t, reason, p.reason)
					}
				})
			})
		}
	}
}

func TestSuccessfulDeliverySurvivesShutdown(t *testing.T) {
	for _, notice := range []bool{false, true} {
		t.Run(map[bool]string{false: "input", true: "notice"}[notice], func(t *testing.T) {
			f := newDeliveryRig(t, Config{})
			var sends atomic.Int32
			send := func() error { sends.Add(1); f.cancel(); return nil }
			f.model.send = func(context.Context, domain.UserInput) error { return send() }
			f.config.NotifyOwner = func(context.Context, string) error { return send() }
			stop := f.start(t)
			item := domain.InboxItem{SourceKey: "sent", SessionKey: "session", Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: "once"}, Context: domain.SessionContext{}}}
			if notice {
				item.Payload.Notice = "once"
			}
			id, err := f.inbox.Enqueue(t.Context(), item)
			require.NoError(t, err)
			f.runner.Notify()
			require.Eventually(t, func() bool { return sends.Load() == 1 }, time.Second, time.Millisecond)
			stop()
			f.status(t, id, "done")
			stop = f.start(t)
			defer stop()
			require.Never(t, func() bool { return sends.Load() != 1 }, 30*time.Millisecond, time.Millisecond)
		})
	}
}

func TestAutonomousTurnHasDeadlineWithoutNewInboxItem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{TurnTimeout: 150 * time.Millisecond})
		f.start(t)
		id := f.enqueue(t, "room", "start background work")
		s := take(t, f.model.opened)
		take(t, s.inputs)
		f.status(t, id, "done")
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		f.turn(t, "room", false)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventBackgroundDone}
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnStarted}
		saved := f.turn(t, "room", true)
		require.WithinDuration(t, time.Now().Add(150*time.Millisecond), saved.Turn.Deadline, 100*time.Millisecond)
		take(t, s.closed)
		f.status(t, id, "done")
		var count int
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox").Scan(&count))
		require.Equal(t, 1, count)
	})
}
