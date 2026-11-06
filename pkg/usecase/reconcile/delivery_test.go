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
	f.runner = New(f.config, f.inbox, repository.NewScheduleRepository(f.db), f.sessions, repository.NewEventRepository(f.db), f.model, &hubFake{closed: make(chan domain.SessionKey, 100)})
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
			var attempts atomic.Int32
			blockFirst := func(ctx context.Context) error {
				if attempts.Add(1) == 1 {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
			cfg := Config{TurnTimeout: 40 * time.Millisecond, MaxAttempts: 1}
			if notice {
				cfg.NotifyOwner = func(ctx context.Context, _ string) error { return blockFirst(ctx) }
			} else {
				cfg.Prepare = func(ctx context.Context, item domain.InboxItem) (domain.UserInput, error) {
					return item.Payload.Input, blockFirst(ctx)
				}
			}
			f := newDeliveryRig(t, cfg)
			f.start(t)
			item := domain.InboxItem{SourceKey: "unsent", SessionKey: "session", Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: "original"}, Context: domain.SessionContext{}}}
			if notice {
				item.Payload.Notice = "owner note"
			}
			id, err := f.inbox.Enqueue(t.Context(), item)
			require.NoError(t, err)
			f.runner.Notify()
			if !notice {
				s := take(t, f.model.opened)
				require.Equal(t, "original", take(t, s.inputs).Text)
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
			}
			f.status(t, id, "done")
			require.EqualValues(t, 2, attempts.Load())
			var failures int
			require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&failures))
			require.Zero(t, failures)
		})
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
}
