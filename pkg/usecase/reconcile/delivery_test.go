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
func (f *deliveryRig) turn(t *testing.T, busy bool) domain.Session {
	t.Helper()
	var saved domain.Session
	require.Eventually(t, func() bool {
		var err error
		saved, err = f.sessions.Get(t.Context(), domain.MainSessionKey)
		return err == nil && (saved.Turn != nil) == busy
	}, time.Second, time.Millisecond)
	return saved
}

func TestClosingProcessReservesSessionKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryRig(t, Config{})
		release := make(chan struct{})
		f.model.closeWait = release
		stop := f.start(t)
		unblock := sync.OnceFunc(func() { close(release) })
		defer func() { unblock(); stop() }()
		id := f.enqueue(t, "room", "first")
		s := take(t, f.model.opened)
		take(t, s.inputs)
		f.status(t, id, "done")
		close(s.events)
		take(t, s.closed)
		next := f.enqueue(t, "room", "after exit")
		synctest.Wait()
		require.Empty(t, f.model.opened)
		unblock()
		s = take(t, f.model.opened)
		require.Equal(t, "after exit", take(t, s.inputs).Text)
		f.status(t, next, "done")
	})
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

func TestConversationFollowupRequiresAcceptRuleBeforeOpen(t *testing.T) {
	var prepared atomic.Int32
	f := newDeliveryRig(t, Config{
		Accept: func(info domain.SessionContext) bool { return info.Conversation != "removed" },
		Prepare: func(_ context.Context, item domain.InboxItem) (domain.UserInput, error) {
			prepared.Add(1)
			return item.Payload.Input, nil
		},
	})
	f.start(t)
	id := f.enqueue(t, "removed", "followup")
	f.status(t, id, "dead")
	require.Empty(t, f.model.opened)
	require.Zero(t, prepared.Load())
	var attempts int
	var reason string
	require.NoError(t, f.db.QueryRow("SELECT attempts,last_error FROM inbox WHERE id=?", id).Scan(&attempts, &reason))
	require.Zero(t, attempts)
	require.Equal(t, "session no longer has an accept rule", reason)
}

func TestTaskProcedureReplacesInput(t *testing.T) {
	const text = "procedure instructions"
	f := newDeliveryRig(t, Config{
		Procedure: func(_ context.Context, name string) (string, error) {
			require.Equal(t, "heartbeat", name)
			return text, nil
		},
	})
	require.NoError(t, f.sessions.Save(t.Context(), domain.Session{Key: domain.MainSessionKey, ResumeToken: "saved"}))
	f.start(t)
	item := TaskItem("heartbeat", "run", "")
	item.Payload.Procedure = "heartbeat"
	id, err := f.inbox.Enqueue(t.Context(), item)
	require.NoError(t, err)
	f.runner.Notify()
	s := take(t, f.model.opened)
	require.Equal(t, text, take(t, s.inputs).Text)
	f.status(t, id, "done")
	var attempts int
	require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
	require.Zero(t, attempts)
}

func TestScheduleToolRequiresFiveFields(t *testing.T) {
	r := New(Config{}, nil, nil, nil, nil, nil, nil)
	for _, expression := range []string{"CRON_TZ=UTC 0 4 * * *", "TZ=UTC\t*\t*\t*\t*"} {
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
	f.turn(t, true)
	select {
	case <-s.closed:
		t.Fatal("rejected input killed accepted turn")
	default:
	}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.turn(t, false)
	third := f.enqueue(t, "room", "third accepted")
	require.Equal(t, "third accepted", take(t, s.inputs).Text)
	f.status(t, third, "done")
	require.Empty(t, f.model.opened)
}

func TestInterruptedTurnContinuesOnlyOnce(t *testing.T) {
	for _, lane := range []string{"conv", "task"} {
		t.Run(lane, func(t *testing.T) {
			f := newDeliveryRig(t, Config{})
			f.start(t)
			item := TaskItem("interrupted", "work", "")
			if lane == "conv" {
				item.SessionKey, item.Lane = domain.MainSessionKey, "conv"
				item.Payload.Context = domain.SessionContext{Conversation: "room"}
			}
			id, err := f.inbox.Enqueue(t.Context(), item)
			require.NoError(t, err)
			f.runner.Notify()
			s := take(t, f.model.opened)
			take(t, s.inputs)
			f.status(t, id, "done")
			close(s.events)
			s = take(t, f.model.opened)
			require.Contains(t, take(t, s.inputs).Text, "previous turn was interrupted")
			require.Equal(t, "resume-token", s.config.ResumeToken)
			require.Eventually(t, func() bool {
				saved, err := f.sessions.Get(t.Context(), domain.MainSessionKey)
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
		})
	}
}

func TestSuccessfulDeliverySurvivesShutdown(t *testing.T) {
	for _, notice := range []bool{false, true} {
		t.Run(map[bool]string{false: "input", true: "notice"}[notice], func(t *testing.T) {
			f := newDeliveryRig(t, Config{})
			var sends atomic.Int32
			send := func() error { sends.Add(1); f.cancel(); return nil }
			f.model.send = func(_ context.Context, input domain.UserInput) error {
				if input.Text == "once" {
					return send()
				}
				return nil
			}
			f.config.NotifyOwner = func(context.Context, string) error { return send() }
			stop := f.start(t)
			item := domain.InboxItem{SourceKey: "sent", SessionKey: domain.MainSessionKey, Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: "once"}, Context: domain.SessionContext{}}}
			if notice {
				item = domain.NewOwnerNotice("sent", "once")
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

func TestDeliveryFailuresKeepBackoff(t *testing.T) {
	for _, notice := range []bool{false, true} {
		t.Run(map[bool]string{false: "input", true: "notice"}[notice], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				notes := make(chan string, 4)
				started := make(chan time.Time, 4)
				fail := func() error { started <- time.Now(); return errors.New("delivery failed") }
				cfg := Config{RetryDelay: 100 * time.Millisecond, MaxAttempts: 3}
				cfg.NotifyOwner = func(_ context.Context, text string) error {
					if text == "owner note" {
						return fail()
					}
					notes <- text
					return nil
				}
				f := newDeliveryRig(t, cfg)
				f.model.send = func(context.Context, domain.UserInput) error { return fail() }
				stop := f.start(t)
				item := domain.InboxItem{SourceKey: "unsent", SessionKey: domain.MainSessionKey, Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: "original"}}}
				if notice {
					item = domain.NewOwnerNotice("unsent", "owner note")
				}
				id, err := f.inbox.Enqueue(t.Context(), item)
				require.NoError(t, err)
				f.runner.Notify()
				var retry time.Time
				for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
					at := take(t, started)
					if attempt > 1 {
						require.True(t, retry.Equal(at))
					}
					f.status(t, id, map[bool]string{false: "pending", true: "dead"}[attempt == cfg.MaxAttempts])
					var attempts int
					require.NoError(t, f.db.QueryRow("SELECT attempts,retry_at FROM inbox WHERE id=?", id).Scan(&attempts, &retry))
					require.Equal(t, attempt, attempts)
					require.True(t, retry.Equal(at.Add(cfg.RetryDelay*time.Duration(1<<(attempt-1)))))
					if attempt < cfg.MaxAttempts {
						time.Sleep(time.Until(retry))
					}
				}
				if !notice {
					require.Contains(t, take(t, notes), "failed after 3 attempts: delivery failed")
				}
				synctest.Wait()
				stop()
				active, err := f.inbox.Active(t.Context())
				require.NoError(t, err)
				require.Empty(t, active)
				require.Empty(t, notes)
			})
		})
	}
}
