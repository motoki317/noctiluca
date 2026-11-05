package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

type fakeMessaging struct {
	in  chan domain.InboundEvent
	out chan domain.SentMessage
}

func (f *fakeMessaging) Start(ctx context.Context, consume func(context.Context, domain.InboundEvent) error) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-f.in:
			if err := consume(ctx, e); err != nil {
				return err
			}
		}
	}
}
func (f *fakeMessaging) Send(_ context.Context, to domain.ConversationRef, message domain.OutboundMessage) (string, error) {
	f.out <- domain.SentMessage{ID: "sent", Conversation: to, Message: message}
	return "sent", nil
}
func (*fakeMessaging) Tools() []domain.Tool { return nil }
func (*fakeMessaging) ToolNotes() string    { return "" }

type fakeModel struct {
	opened chan *fakeSession
	fail   bool
	send   func(context.Context, domain.UserInput) error
}

func (f *fakeModel) Open(_ context.Context, config domain.ModelSessionConfig) (domain.ModelSession, error) {
	if f.fail {
		return nil, errors.New("open failed")
	}
	s := &fakeSession{config: config, inputs: make(chan domain.UserInput, 10), events: make(chan domain.ModelEvent, 10), closed: make(chan struct{}), send: f.send}
	s.idle.Store(true)
	f.opened <- s
	return s, nil
}

type fakeSession struct {
	config domain.ModelSessionConfig
	inputs chan domain.UserInput
	events chan domain.ModelEvent
	closed chan struct{}
	once   sync.Once
	idle   atomic.Bool
	send   func(context.Context, domain.UserInput) error
}

func (s *fakeSession) Send(ctx context.Context, in domain.UserInput) error {
	if s.send != nil {
		return s.send(ctx, in)
	}
	s.idle.Store(false)
	s.inputs <- in
	return nil
}
func (s *fakeSession) Events() <-chan domain.ModelEvent { return s.events }
func (s *fakeSession) ResumeToken() string              { return "resume-" + string(s.config.Key) }
func (s *fakeSession) Idle() bool                       { return s.idle.Load() }
func (s *fakeSession) Close() error {
	s.once.Do(func() { close(s.events); close(s.closed) })
	return nil
}

type fakeHub struct {
	mu     sync.Mutex
	tools  map[string]domain.Tool
	actors map[domain.SessionKey]domain.Actor
	closed chan domain.SessionKey
	count  int
}

func (*fakeHub) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (*fakeHub) Shutdown(context.Context) error  { return nil }
func (f *fakeHub) ReplaceSource(name string, tools []domain.Tool) error {
	return f.AddSource(name, tools)
}
func (f *fakeHub) AddSource(_ string, tools []domain.Tool) error {
	for _, t := range tools {
		f.tools[t.Name] = t
	}
	return nil
}
func (f *fakeHub) OpenSession(key domain.SessionKey, actor domain.Actor) (domain.MCPEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors[key] = actor
	f.count++
	return domain.MCPEndpoint{URL: "http://localhost/mcp", BearerToken: fmt.Sprint(f.count)}, nil
}
func (f *fakeHub) CloseSession(key domain.SessionKey) {
	f.mu.Lock()
	delete(f.actors, key)
	f.mu.Unlock()
	f.closed <- key
}
func (f *fakeHub) actor(key domain.SessionKey) domain.Actor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.actors[key]
}

type fixture struct {
	core      domain.Core
	messaging *fakeMessaging
	model     *fakeModel
	hub       *fakeHub
	events    domain.EventRepository
	sessions  domain.SessionRepository
	done      chan error
	wantError string
}

func setup(t *testing.T, config Config, prompt domain.PromptAssembler) *fixture {
	return setupGroups(t, config, prompt, nil)
}

func setupGroups(t *testing.T, config Config, prompt domain.PromptAssembler, groups func(context.Context, []string) (map[string][]string, error)) *fixture {
	return setupExternal(t, config, prompt, groups, nil)
}

func setupExternal(t *testing.T, config Config, prompt domain.PromptAssembler, groups func(context.Context, []string) (map[string][]string, error), external domain.ExternalTools) *fixture {
	return setupMemory(t, config, prompt, groups, external, nil)
}

func setupMemory(t *testing.T, config Config, prompt domain.PromptAssembler, groups func(context.Context, []string) (map[string][]string, error), external domain.ExternalTools, memory domain.MemoryStore) *fixture {
	t.Helper()
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "history.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	f := &fixture{messaging: &fakeMessaging{in: make(chan domain.InboundEvent), out: make(chan domain.SentMessage, 10)}, model: &fakeModel{opened: make(chan *fakeSession, 10)},
		hub: &fakeHub{tools: make(map[string]domain.Tool), actors: make(map[domain.SessionKey]domain.Actor), closed: make(chan domain.SessionKey, 10)}, events: repository.NewEventRepository(db), sessions: repository.NewSessionRepository(db), done: make(chan error, 1)}
	var messaging domain.MessagingAdapter = f.messaging
	if groups != nil {
		messaging = &groupMessaging{fakeMessaging: f.messaging, groups: groups}
	}
	f.core, err = New(config, messaging, f.model, f.hub, f.events, f.sessions, prompt, repository.NewScheduleRepository(db), external, memory, repository.NewInboxRepository(db))
	require.NoError(t, err)
	go func() { f.done <- f.core.Start(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, f.core.Shutdown(ctx))
		if f.wantError != "" {
			require.ErrorContains(t, take(t, f.done), f.wantError)
		} else {
			require.NoError(t, take(t, f.done))
		}
	})
	return f
}
func take[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
		var zero T
		return zero
	}
}
func message(to domain.ConversationRef, user, text string) domain.InboundEvent {
	return domain.InboundEvent{Kind: domain.InboundEventMessage, Conversation: to, Sender: domain.Sender{ID: user, Kind: domain.SenderHuman}, Text: text, ReceivedAt: time.Now()}
}

func TestConversationFlow(t *testing.T) {
	var prompts atomic.Int32
	f := setup(t, Config{Owner: "owner", Rules: map[domain.ConversationRef]domain.AcceptRule{"room": {Users: []string{"owner", "member"}}, "dm": {Users: []string{"owner"}}}},
		func(_ context.Context, identity domain.ToolSession, in domain.UserInput) (domain.UserInput, error) {
			prompts.Add(1)
			in.Text = "fresh context\n" + in.Text
			return in, nil
		})
	f.messaging.in <- message("room", "owner", "first")
	s := take(t, f.model.opened)
	require.Contains(t, take(t, s.inputs).Text, "fresh context")
	require.Equal(t, domain.ActorMember, f.hub.actor("room"))
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnStarted}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.messaging.in <- message("room", "member", "steer")
	require.Contains(t, take(t, s.inputs).Text, "steer")
	require.EqualValues(t, 2, prompts.Load())
	select {
	case <-f.model.opened:
		t.Fatal("opened another model")
	default:
	}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventText, Text: "never post this"}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventBackgroundDone, Text: "background done"}
	ctx := domain.WithToolSession(t.Context(), domain.ToolSession{Key: "room", Actor: domain.ActorMember})
	result, err := f.hub.tools["reply"].Handler(ctx, json.RawMessage(`{"text":"visible"}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"sent"}`, string(result))
	require.Equal(t, "visible", take(t, f.messaging.out).Message.Text)
	sends, err := f.events.ListOutbound(t.Context(), "room", 30)
	require.NoError(t, err)
	require.Len(t, sends, 1)
	f.messaging.in <- message("room", "denied", "still recorded")
	f.messaging.in <- message("dm", "owner", "private")
	dm := take(t, f.model.opened)
	take(t, dm.inputs)
	require.Equal(t, domain.ActorOwner, f.hub.actor("dm"))
	inbound, err := f.events.ListInbound(t.Context(), "room", 30)
	require.NoError(t, err)
	require.Len(t, inbound, 3)
	require.NoError(t, s.Close())
	require.Equal(t, domain.SessionKey("room"), take(t, f.hub.closed))
	records, err := f.events.ListModel(t.Context(), "room", 30)
	require.NoError(t, err)
	require.Len(t, records, 5)
	require.Equal(t, domain.ModelEventBackgroundDone, records[0].Event.Kind)
	select {
	case posted := <-f.messaging.out:
		t.Fatalf("unexpected post: %+v", posted)
	default:
	}
}

func TestThreadHistoryIdleAndResume(t *testing.T) {
	f := setup(t, Config{IdleTimeout: 30 * time.Millisecond, Rules: map[domain.ConversationRef]domain.AcceptRule{"room": {Users: []string{"member"}}}}, nil)
	for i := range 35 {
		e := message("room", "denied", fmt.Sprintf("parent-%02d", i))
		require.NoError(t, f.events.SaveInbound(t.Context(), e))
	}
	e := message("thread", "member", "new thread")
	e.Parent = "room"
	f.messaging.in <- e
	s := take(t, f.model.opened)
	input := take(t, s.inputs)
	require.NotContains(t, input.Text, "parent-04")
	require.Contains(t, input.Text, "parent-05")
	require.Contains(t, input.Text, "parent-34")
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	// A completed turn can still own background work and its tool grant.
	select {
	case <-s.closed:
		t.Fatal("closed a busy session")
	case <-time.After(80 * time.Millisecond):
	}
	require.Equal(t, domain.ActorMember, f.hub.actor("thread"))
	s.idle.Store(true)
	s.events <- domain.ModelEvent{Kind: domain.ModelEventBackgroundDone, Text: "done"}
	require.Equal(t, domain.SessionKey("thread"), take(t, f.hub.closed))
	saved, err := f.sessions.Get(t.Context(), "thread")
	require.NoError(t, err)
	require.Equal(t, "resume-thread", saved.ResumeToken)
	f.messaging.in <- e
	resumed := take(t, f.model.opened)
	require.Equal(t, "resume-thread", resumed.config.ResumeToken)
	require.NotEqual(t, s.config.ToolHub.BearerToken, resumed.config.ToolHub.BearerToken)
	require.NotContains(t, take(t, resumed.inputs).Text, "parent_history")
}

func TestOpenFailureRevokesGrant(t *testing.T) {
	f := setup(t, Config{Rules: map[domain.ConversationRef]domain.AcceptRule{"room": {Users: []string{"member"}}}}, nil)
	f.model.fail = true

	f.messaging.in <- message("room", "member", "hello")
	require.Equal(t, domain.SessionKey("room"), take(t, f.hub.closed))
	require.Empty(t, f.hub.actor("room"))
	events, err := f.events.ListInbound(t.Context(), "room", 1)
	require.NoError(t, err)
	require.Len(t, events, 1)
}

func TestAcceptRules(t *testing.T) {
	h := &fakeHub{tools: make(map[string]domain.Tool)}
	m := &fakeMessaging{}
	config := Config{Owner: "owner", UserGroups: map[string][]string{"team": {"owner", "member"}}, Rules: map[domain.ConversationRef]domain.AcceptRule{
		"room": {Groups: []string{"team"}, RequireMention: true}, "bots": {Users: []string{"bot"}, AllowBots: true}, "dm": {Users: []string{"owner"}}, "blocked": {}}}
	c, err := New(config, m, &fakeModel{}, h, &unusedEvents{}, &unusedSessions{}, nil, &unusedSchedules{}, nil, nil, &unusedInbox{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Shutdown(context.Background())) })
	impl := c.(*core)
	for _, tc := range []struct {
		to                 domain.ConversationRef
		user               string
		bot, mention, want bool
	}{
		{"room", "member", false, true, true}, {"room", "member", false, false, false}, {"room", "outsider", false, true, false},
		{"room", "member", true, true, false}, {"bots", "bot", true, false, true}, {"dm", "owner", false, false, true}, {"blocked", "owner", false, false, false}, {"unknown", "owner", false, false, false},
	} {
		e := message(tc.to, tc.user, "")
		e.MentionsMe = tc.mention
		if tc.bot {
			e.Sender.Kind = domain.SenderBot
		}
		_, ok := impl.accepted(e)
		require.Equal(t, tc.want, ok)
	}
	config.Rules["dm"] = domain.AcceptRule{Users: []string{"member"}}
	r, ok := impl.accepted(message("dm", "owner", ""))
	require.True(t, ok)
	require.Equal(t, domain.ActorOwner, r.actor)
	thread := message("thread", "member", "")
	thread.Parent = "room"
	thread.MentionsMe = true
	_, ok = impl.accepted(thread)
	require.True(t, ok)
	require.Equal(t, domain.SessionKey("thread"), domain.SessionKey(thread.Conversation))
}

type unusedEvents struct{ domain.EventRepository }
type unusedSessions struct{ domain.SessionRepository }
type unusedSchedules struct{ domain.ScheduleRepository }

func (*fakeMessaging) Shutdown(context.Context) error { return nil }

func TestShutdownDuringInput(t *testing.T) {
	f := setup(t, Config{Rules: map[domain.ConversationRef]domain.AcceptRule{"room": {Users: []string{"member"}}}}, nil)
	entered := make(chan struct{})
	f.model.send = func(ctx context.Context, _ domain.UserInput) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	f.messaging.in <- message("room", "member", "start")
	take(t, entered)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, f.core.Shutdown(ctx))
}

type unusedInbox struct{ domain.InboxRepository }
