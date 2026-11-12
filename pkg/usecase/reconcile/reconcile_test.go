package reconcile

import (
	"context"
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

type modelFake struct {
	manualAck bool
	idle      bool
	send      func(context.Context, domain.UserInput) error
	opened    chan *sessionFake
	closeWait <-chan struct{}
}

func (m *modelFake) Open(ctx context.Context, c domain.ModelSessionConfig) (domain.ModelSession, error) {
	if c.Key == "fail" {
		return nil, errors.New("model unavailable")
	}
	if c.Key == "slow-open" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s := &sessionFake{config: c, send: m.send, manualAck: m.manualAck, idle: m.idle, closeWait: m.closeWait, inputs: make(chan domain.UserInput, 4), events: make(chan domain.ModelEvent, 8), closed: make(chan struct{})}
	m.opened <- s
	return s, nil
}

type sessionFake struct {
	manualAck bool
	idle      bool
	send      func(context.Context, domain.UserInput) error
	config    domain.ModelSessionConfig
	closeWait <-chan struct{}
	inputs    chan domain.UserInput
	events    chan domain.ModelEvent
	closed    chan struct{}
	once      sync.Once
}

func (s *sessionFake) Send(ctx context.Context, in domain.UserInput) (err error) {
	defer func() {
		if err == nil && !s.manualAck {
			s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: in.ID}
		}
	}()
	if s.send != nil {
		return s.send(ctx, in)
	}
	if in.Text == "send-error" {
		return errors.New("send failed")
	}
	select {
	case s.inputs <- in:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *sessionFake) Events() <-chan domain.ModelEvent { return s.events }
func (s *sessionFake) ResumeToken() string              { return "resume-token" }
func (s *sessionFake) Idle() bool                       { return s.idle }
func (s *sessionFake) Close() error {
	s.once.Do(func() {
		close(s.closed)
		if s.closeWait != nil {
			<-s.closeWait
		}
	})
	return nil
}

type hubFake struct {
	domain.ToolHub
	closed chan domain.SessionKey
}

func (h *hubFake) OpenSession(domain.SessionKey) (domain.MCPEndpoint, error) {
	return domain.MCPEndpoint{}, nil
}
func (h *hubFake) CloseSession(key domain.SessionKey) { h.closed <- key }
func take[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
		var zero T
		return zero
	}
}

func TestOpenFailureRetriesAndRevokesGrants(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notes := make(chan string, 10)
		f := newDeliveryRig(t, Config{RetryDelay: 5 * time.Millisecond, MaxAttempts: 2, NotifyOwner: func(_ context.Context, note string) error { notes <- note; return nil }})
		f.start(t)
		id := f.enqueue(t, "fail", "bad")
		for range 2 {
			require.Equal(t, domain.SessionKey("fail"), take(t, f.hub.closed))
		}
		f.status(t, id, "dead")
		var attempts int
		require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
		require.Equal(t, 2, attempts)
		require.Contains(t, take(t, notes), "model unavailable")
		require.Empty(t, f.model.opened)
	})
}

func TestRejectedConversationRetiresSessionAndRejectsWake(t *testing.T) {
	var rejected atomic.Bool
	f := newDeliveryRig(t, Config{Accept: func(domain.SessionContext) bool { return !rejected.Load() }})
	f.start(t)
	id := f.enqueue(t, "room", "first")
	s := take(t, f.model.opened)
	take(t, s.inputs)
	f.status(t, id, "done")
	rejected.Store(true)
	next := f.enqueue(t, "room", "wake")
	require.Equal(t, domain.SessionKey("room"), take(t, f.hub.closed))
	take(t, s.closed)
	f.status(t, next, "dead")
	record, err := f.sessions.Get(t.Context(), "room")
	require.NoError(t, err)
	require.Equal(t, "resume-token", record.ResumeToken)
	require.Empty(t, f.model.opened)
}

func TestSlowOpenDoesNotBlockAnotherConversation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
		require.NoError(t, err)
		defer db.Close()
		inbox := repository.NewInboxRepository(db)
		for _, key := range []string{"slow-open", "fast"} {
			item := TaskItem(key, "input", "")
			item.SessionKey = domain.SessionKey(key)
			item.Lane = "conv"
			item.Payload.Context.TaskName = ""
			_, err := inbox.Enqueue(t.Context(), item)
			require.NoError(t, err)
		}
		m := &modelFake{opened: make(chan *sessionFake, 2)}
		h := &hubFake{closed: make(chan domain.SessionKey, 4)}
		r := New(Config{TurnTimeout: 100 * time.Millisecond, MaxAttempts: 1}, inbox, repository.NewScheduleRepository(db), repository.NewSessionRepository(db), repository.NewEventRepository(db), m, h)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Start(ctx) }()
		s := take(t, m.opened)
		require.Equal(t, domain.SessionKey("fast"), s.config.Key)
		take(t, s.inputs)
		s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		require.Equal(t, domain.SessionKey("slow-open"), take(t, h.closed))
		cancel()
		require.NoError(t, take(t, done))
	})
}

func TestNoticeRetrySurvivesRestart(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	defer db.Close()
	inbox := repository.NewInboxRepository(db)
	item := TaskItem("failed", "work", "")
	id, err := inbox.Enqueue(t.Context(), item)
	require.NoError(t, err)
	_, err = inbox.Claim(t.Context(), id, time.Now())
	require.NoError(t, err)
	_, err = inbox.Finish(t.Context(), id, "bad model", 1, time.Now())
	require.NoError(t, err)
	makeRunner := func(notify func(context.Context, string) error) *Reconciler {
		return New(Config{RetryDelay: 10 * time.Millisecond, NotifyOwner: notify}, inbox, repository.NewScheduleRepository(db), repository.NewSessionRepository(db), repository.NewEventRepository(db), &modelFake{opened: make(chan *sessionFake, 1)}, &hubFake{closed: make(chan domain.SessionKey, 4)})
	}
	first := makeRunner(func(context.Context, string) error { return errors.New("network failed") })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- first.Start(ctx) }()
	require.Eventually(t, func() bool {
		items, err := inbox.Active(t.Context())
		return err == nil && len(items) == 1 && items[0].Attempts == 1 && items[0].Status == "pending"
	}, time.Second, time.Millisecond)
	cancel()
	require.NoError(t, take(t, done))
	notes := make(chan string, 1)
	second := makeRunner(func(_ context.Context, n string) error { notes <- n; return nil })
	ctx, cancel = context.WithCancel(t.Context())
	go func() { done <- second.Start(ctx) }()
	require.Contains(t, take(t, notes), "bad model")
	require.Eventually(t, func() bool { items, err := inbox.Active(t.Context()); return err == nil && len(items) == 0 }, time.Second, time.Millisecond)
	cancel()
	require.NoError(t, take(t, done))
}
func TestToolScheduleUsesDefaultWorkdir(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	defer db.Close()
	schedules := repository.NewScheduleRepository(db)
	r := New(Config{Workdir: "/work"}, nil, schedules, nil, nil, nil, nil)
	_, err = r.Tools()[0].Handler(t.Context(), []byte(`{"name":"daily","cron":"0 4 * * *","tz":"UTC","prompt":"work"}`))
	require.NoError(t, err)
	jobs, err := schedules.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, "/work", jobs[0].Workdir)
}
