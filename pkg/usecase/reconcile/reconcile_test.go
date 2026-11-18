package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

func testProcedure(_ context.Context, name string) (string, error) { return "procedure: " + name, nil }

type modelFake struct {
	manualAck bool
	idle      bool
	openErr   error
	send      func(context.Context, domain.UserInput) error
	opened    chan *sessionFake
	closeWait <-chan struct{}
}

func (m *modelFake) Open(ctx context.Context, c domain.ModelSessionConfig) (domain.ModelSession, error) {
	if m.openErr != nil {
		return nil, m.openErr
	}
	s := &sessionFake{config: c, send: m.send, manualAck: m.manualAck, idle: m.idle, closeWait: m.closeWait, inputs: make(chan domain.UserInput, 4), events: make(chan domain.ModelEvent, 8), closed: make(chan struct{})}
	m.opened <- s
	return s, nil
}

type sessionFake struct {
	manualAck bool
	idle      bool
	busy      atomic.Bool
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
			s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnStarted}
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
func (s *sessionFake) Idle() bool                       { return s.idle && !s.busy.Load() }
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

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestModelLifecycleLogs(t *testing.T) {
	for _, tt := range []struct {
		name       string
		failure    string
		adapterErr string
		reason     string
	}{
		{name: "failed turn and shutdown", failure: "model unavailable", adapterErr: "background turn failed", reason: "shutdown"},
		{name: "adapter error and process exit", adapterErr: "process exited unexpectedly", reason: "model process exited"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLogs(t)
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryRig(t, Config{NotifyOwner: func(context.Context, string) error { return nil }})
				require.NoError(t, f.sessions.Save(t.Context(), domain.Session{Key: domain.MainSessionKey, ResumeToken: "saved-token"}))
				stop := f.start(t)
				id := f.enqueue(t, "room", "private-input-text")
				s := take(t, f.model.opened)
				take(t, s.inputs)
				f.status(t, id, "done")
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnStarted}
				if tt.failure != "" {
					s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New(tt.failure)}
				}
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				f.turn(t, false)
				synctest.Wait()
				require.Empty(t, f.runner.live[domain.MainSessionKey].lastError)
				finish := map[string]any{"level": "INFO", "msg": "Turn finished", "session": "main"}
				exit := map[string]any{"level": "INFO", "msg": "Model process exited", "session": "main", "reason": tt.reason}
				if tt.failure != "" {
					finish["error"] = tt.failure
				}
				if tt.reason == "model process exited" {
					exit["error"] = tt.adapterErr
				}
				s.events <- domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New(tt.adapterErr)}
				s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
				synctest.Wait()
				if tt.reason == "model process exited" {
					close(s.events)
					synctest.Wait()
				}
				stop()

				var records []map[string]any
				for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
					var record map[string]any
					require.NoError(t, json.Unmarshal(line, &record))
					delete(record, "time")
					records = append(records, record)
				}
				require.Equal(t, []map[string]any{
					{"level": "INFO", "msg": "Model process started", "session": "main"},
					{"level": "INFO", "msg": "Turn started", "session": "main", "item": float64(id)},
					finish, exit,
				}, records)
			})
		})
	}
}

func TestNoticeHasNoModelLifecycleLogs(t *testing.T) {
	logs := captureLogs(t)
	f := newDeliveryRig(t, Config{NotifyOwner: func(context.Context, string) error { return nil }})
	id, err := f.inbox.Enqueue(t.Context(), domain.NewOwnerNotice("notice", "private-notice-text"))
	require.NoError(t, err)
	stop := f.start(t)
	f.status(t, id, "done")
	stop()
	require.Empty(t, f.model.opened)
	require.Empty(t, logs.String())
}

func TestTurnHasNoDeadlineAndCrashContinuesOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notes := make(chan string, 4)
		f := newDeliveryRig(t, Config{NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil }})
		f.start(t)
		id, err := f.inbox.Enqueue(t.Context(), TaskItem("long", "silent command", ""))
		require.NoError(t, err)
		f.runner.Notify()
		s := take(t, f.model.opened)
		require.Equal(t, id, take(t, s.inputs).ID)
		f.status(t, id, "done")
		before := f.turn(t, true)
		time.Sleep(3 * time.Hour)
		synctest.Wait()
		require.Equal(t, before.Turn, f.turn(t, true).Turn)
		require.Empty(t, notes)
		require.Empty(t, s.closed)
		require.Empty(t, f.hub.closed)
		require.Empty(t, f.model.opened)

		close(s.events)
		take(t, s.closed)
		s = take(t, f.model.opened)
		require.Equal(t, "resume-token", s.config.ResumeToken)
		continued := take(t, s.inputs)
		require.Contains(t, continued.Text, "previous turn was interrupted: model process exited")
		f.status(t, continued.ID, "done")
		require.Equal(t, 1, f.turn(t, true).Turn.Continuations)
		close(s.events)
		take(t, s.closed)
		synctest.Wait()
		var continuations int
		require.NoError(t, f.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'interrupted:%'").Scan(&continuations))
		require.Equal(t, 1, continuations)
		require.Empty(t, f.model.opened)
		require.Empty(t, notes)
	})
}

func TestOpenFailureRetriesAndRevokesGrants(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notes := make(chan string, 10)
		f := newDeliveryRig(t, Config{RetryDelay: 5 * time.Millisecond, MaxAttempts: 2, NotifyOwner: func(_ context.Context, note string) error { notes <- note; return nil }})
		f.model.openErr = errors.New("model unavailable")
		f.start(t)
		id := f.enqueue(t, "fail", "bad")
		for range 2 {
			require.Equal(t, domain.MainSessionKey, take(t, f.hub.closed))
		}
		f.status(t, id, "dead")
		var attempts int
		require.NoError(t, f.db.QueryRow("SELECT attempts FROM inbox WHERE id=?", id).Scan(&attempts))
		require.Equal(t, 2, attempts)
		require.Contains(t, take(t, notes), "model unavailable")
		require.Empty(t, f.model.opened)
	})
}

func TestRejectedConversationKeepsSessionAndRejectsRow(t *testing.T) {
	var rejected atomic.Bool
	f := newDeliveryRig(t, Config{Accept: func(domain.SessionContext) bool { return !rejected.Load() }})
	f.start(t)
	id := f.enqueue(t, "room", "first")
	s := take(t, f.model.opened)
	take(t, s.inputs)
	f.status(t, id, "done")
	rejected.Store(true)
	next := f.enqueue(t, "room", "wake")
	f.status(t, next, "dead")
	record, err := f.sessions.Get(t.Context(), domain.MainSessionKey)
	require.NoError(t, err)
	require.Equal(t, "resume-token", record.ResumeToken)
	require.Empty(t, f.model.opened)
	require.Empty(t, s.closed)
}

func TestNoticeRetrySurvivesRestart(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	defer db.Close()
	inbox := repository.NewInboxRepository(db)
	item := TaskItem("failed", "work", "")
	id, err := inbox.Enqueue(t.Context(), item)
	require.NoError(t, err)
	_, err = inbox.Claim(t.Context(), id)
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

func TestAllInputsReachMainInReadyOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notes := make(chan string, 1)
		f := newDeliveryRig(t, Config{Workdir: "/default", NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil }})
		f.model.manualAck = true
		legacy := domain.Session{Key: "room-a", ResumeToken: "old-token", Turn: &domain.SessionTurn{}}
		require.NoError(t, f.sessions.Save(t.Context(), legacy))
		at := time.Now()
		items := []domain.InboxItem{
			{SourceKey: "second", SessionKey: "room-b", Lane: "conv", ReadyAt: at.Add(-time.Second)},
			{SourceKey: "first", SessionKey: "room-a", Lane: "conv", ReadyAt: at.Add(-2 * time.Second)},
			TaskItem("daily", "task", "/task"),
			{SourceKey: "watch:1", SessionKey: "task:old", Lane: "task", ReadyAt: at},
			{SourceKey: "wake:1", SessionKey: "room-b", Lane: "conv", ReadyAt: at.Add(time.Minute)},
		}
		items[2].SessionKey, items[2].ReadyAt = "task:daily", at.Add(-time.Second)
		for i := range items {
			items[i].Payload.Input.Text = items[i].SourceKey
			id, err := f.inbox.Enqueue(t.Context(), items[i])
			require.NoError(t, err)
			items[i].ID = id
		}
		_, err := f.inbox.Enqueue(t.Context(), domain.NewOwnerNotice("notice-test", "owner notice"))
		require.NoError(t, err)
		f.start(t)
		s := take(t, f.model.opened)
		require.Equal(t, domain.MainSessionKey, s.config.Key)
		require.Equal(t, "/default", s.config.Workdir)
		require.Empty(t, s.config.ResumeToken)
		for _, i := range []int{1, 0, 2, 3} {
			require.Equal(t, items[i].ID, take(t, s.inputs).ID)
		}
		synctest.Wait()
		require.Empty(t, s.inputs)
		require.Equal(t, "owner notice", take(t, notes))
		time.Sleep(time.Minute)
		require.Equal(t, items[4].ID, take(t, s.inputs).ID)
		require.Empty(t, f.model.opened)
		saved, err := f.sessions.Get(t.Context(), legacy.Key)
		require.NoError(t, err)
		require.Equal(t, legacy, saved)
		for _, item := range items {
			s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: item.ID}
			f.status(t, item.ID, "done")
		}
	})
}

func TestMainExitRedeliversOldKeyInput(t *testing.T) {
	f := newDeliveryRig(t, Config{})
	f.model.manualAck = true
	f.start(t)
	id := f.enqueue(t, "legacy-room", "unstarted")
	s := take(t, f.model.opened)
	require.Equal(t, id, take(t, s.inputs).ID)
	f.status(t, id, "dispatched")
	close(s.events)
	resumed := take(t, f.model.opened)
	require.Equal(t, domain.MainSessionKey, resumed.config.Key)
	require.Equal(t, "resume-token", resumed.config.ResumeToken)
	require.Equal(t, id, take(t, resumed.inputs).ID)
	resumed.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}
	f.status(t, id, "done")
	var key string
	var attempts int
	require.NoError(t, f.db.QueryRow("SELECT session_key, attempts FROM inbox WHERE id=?", id).Scan(&key, &attempts))
	require.Equal(t, "legacy-room", key)
	require.Zero(t, attempts)
}

func TestNoticeRoutingMatchesRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notes := make(chan string, 2)
		f := newDeliveryRig(t, Config{NotifyOwner: func(_ context.Context, text string) error { notes <- text; return nil }})
		items := []domain.InboxItem{
			domain.NewOwnerNotice("empty", ""),
			{SourceKey: "legacy", SessionKey: "old-room", Lane: "conv", Payload: domain.InboxPayload{Notice: "unused", Input: domain.UserInput{Text: "work"}}},
		}
		for i := range items {
			items[i].Status = "dispatched"
			var err error
			items[i].ID, err = f.inbox.Enqueue(t.Context(), items[i])
			require.NoError(t, err)
		}
		f.start(t)
		require.Empty(t, take(t, notes))
		f.status(t, items[0].ID, "done")
		s := take(t, f.model.opened)
		require.Equal(t, domain.MainSessionKey, s.config.Key)
		require.Equal(t, items[1].ID, take(t, s.inputs).ID)
		f.status(t, items[1].ID, "done")
		require.Empty(t, f.model.opened)
		require.Empty(t, notes)
	})
}
