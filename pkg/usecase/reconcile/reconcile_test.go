package reconcile

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

type modelFake struct {
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
	s := &sessionFake{config: c, send: m.send, closeWait: m.closeWait, inputs: make(chan domain.UserInput, 4), events: make(chan domain.ModelEvent, 8), closed: make(chan struct{})}
	m.opened <- s
	return s, nil
}

type sessionFake struct {
	send      func(context.Context, domain.UserInput) error
	config    domain.ModelSessionConfig
	closeWait <-chan struct{}
	inputs    chan domain.UserInput
	events    chan domain.ModelEvent
	closed    chan struct{}
	once      sync.Once
}

func (s *sessionFake) Send(ctx context.Context, in domain.UserInput) error {
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
func (s *sessionFake) Idle() bool                       { return false }
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

func TestRecoveryFailureAndTaskDeadline(t *testing.T) {
	// Runner scheduling must not consume the deadline used to expire the hung task.
	synctest.Test(t, func(t *testing.T) {
		db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
		require.NoError(t, err)
		defer db.Close()
		inbox := repository.NewInboxRepository(db)
		events := repository.NewEventRepository(db)
		sessions := repository.NewSessionRepository(db)
		enqueue := func(key, text, lane string) int64 {
			item := TaskItem(key, text, "")
			item.SessionKey = domain.SessionKey(key)
			item.Lane = lane
			if lane == "conv" {
				item.Payload.Context.TaskName = ""
			}
			id, err := inbox.Enqueue(t.Context(), item)
			require.NoError(t, err)
			return id
		}
		interrupted := enqueue("recovered", "recover me", "conv")
		_, err = inbox.Claim(t.Context(), interrupted, time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.NoError(t, sessions.Save(t.Context(), domain.Session{Key: "recovered", ResumeToken: "saved-token"}))
		failed := enqueue("fail", "bad", "conv")
		enqueue("hung", "hang", "task")
		enqueue("next", "next task", "task")
		m := &modelFake{opened: make(chan *sessionFake, 10)}
		hub := &hubFake{closed: make(chan domain.SessionKey, 20)}
		notes := make(chan string, 10)
		r := New(Config{TaskTimeout: 100 * time.Millisecond, TurnTimeout: time.Second, RetryDelay: 5 * time.Millisecond, MaxAttempts: 2, NotifyOwner: func(_ context.Context, note string) error { notes <- note; return nil }}, inbox, repository.NewScheduleRepository(db), sessions, events, m, hub)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Start(ctx) }()
		defer func() { cancel(); require.NoError(t, take(t, done)) }()
		opened := map[domain.SessionKey]*sessionFake{}
		for len(opened) < 3 {
			s := take(t, m.opened)
			opened[s.config.Key] = s
			take(t, s.inputs)
			if s.config.Key != "hung" {
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
			}
		}
		take(t, opened["hung"].closed)
		require.Eventually(t, func() bool {
			var status string
			var attempts int
			err := db.QueryRow("SELECT status,attempts FROM inbox WHERE id=?", failed).Scan(&status, &attempts)
			return err == nil && status == "dead" && attempts == 2
		}, time.Second, time.Millisecond)
		require.Equal(t, "saved-token", opened["recovered"].config.ResumeToken)
		require.Contains(t, take(t, notes)+take(t, notes), "fail")
		select {
		case err := <-done:
			t.Fatalf("failure stopped reconciler: %v", err)
		default:
		}
	})
}

func TestSlowOpenDoesNotBlockAnotherConversation(t *testing.T) {
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

func TestEmptyProcedureDoesNotOpenModel(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	defer db.Close()
	inbox := repository.NewInboxRepository(db)
	item := TaskItem("heartbeat", "run", "")
	item.SessionKey = "fail"
	item.Payload.Procedure = "heartbeat"
	id, err := inbox.Enqueue(t.Context(), item)
	require.NoError(t, err)
	r := New(Config{Procedure: func(context.Context, string) (string, error) { return "", nil }, Completed: func(context.Context, string, time.Time) error {
		return errors.New("skipped turn must not complete a procedure")
	}}, inbox, repository.NewScheduleRepository(db), repository.NewSessionRepository(db), repository.NewEventRepository(db), &modelFake{opened: make(chan *sessionFake, 1)}, &hubFake{closed: make(chan domain.SessionKey, 2)})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	require.Eventually(t, func() bool {
		var status string
		var attempts int
		err := db.QueryRow("SELECT status,attempts FROM inbox WHERE id=?", id).Scan(&status, &attempts)
		return err == nil && status == "done" && attempts == 0
	}, time.Second, time.Millisecond)
	cancel()
	require.NoError(t, take(t, done))
}
