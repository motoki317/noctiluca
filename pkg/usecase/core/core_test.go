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
func (*fakeMessaging) DirectMessage(_ context.Context, user string) (domain.ConversationRef, error) {
	return domain.ConversationRef("test:dm:" + user), nil
}

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
	mu     sync.Mutex
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
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
		return errors.New("model session closed")
	default:
	}
	s.idle.Store(false)
	s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: in.ID}
	s.inputs <- in
	return nil
}
func (s *fakeSession) Events() <-chan domain.ModelEvent { return s.events }
func (s *fakeSession) ResumeToken() string              { return "resume-" + string(s.config.Key) }
func (s *fakeSession) Idle() bool                       { return s.idle.Load() }
func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.once.Do(func() { close(s.events); close(s.closed) })
	return nil
}

type fakeHub struct {
	mu       sync.Mutex
	tools    map[string]domain.Tool
	sources  map[string][]domain.Tool
	sessions map[domain.SessionKey]bool
	closed   chan domain.SessionKey
	count    int
}

func (*fakeHub) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (*fakeHub) Shutdown(context.Context) error  { return nil }
func (f *fakeHub) ReplaceSource(name string, tools []domain.Tool) error {
	return f.AddSource(name, tools)
}
func (f *fakeHub) AddSource(name string, tools []domain.Tool) error {
	if f.sources == nil {
		f.sources = make(map[string][]domain.Tool)
	}
	if _, exists := f.sources[name]; exists {
		return fmt.Errorf("duplicate source %q", name)
	}
	f.sources[name] = tools
	for _, t := range tools {
		f.tools[t.Name] = t
	}
	return nil
}
func (f *fakeHub) OpenSession(key domain.SessionKey) (domain.MCPEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[key] = true
	f.count++
	return domain.MCPEndpoint{URL: "http://localhost/mcp", BearerToken: fmt.Sprint(f.count)}, nil
}
func (f *fakeHub) CloseSession(key domain.SessionKey) {
	f.mu.Lock()
	delete(f.sessions, key)
	f.mu.Unlock()
	f.closed <- key
}
func (f *fakeHub) active(key domain.SessionKey) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[key]
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

func setup(t *testing.T, config Config, messaging Messaging, prompt domain.PromptAssembler) *fixture {
	return setupGroups(t, config, messaging, prompt, nil)
}

func setupGroups(t *testing.T, config Config, messaging Messaging, prompt domain.PromptAssembler, groups func(context.Context, []string) (map[string][]string, error)) *fixture {
	return setupExternal(t, config, messaging, prompt, groups, nil)
}

func setupExternal(t *testing.T, config Config, messaging Messaging, prompt domain.PromptAssembler, groups func(context.Context, []string) (map[string][]string, error), external domain.ExternalTools) *fixture {
	return setupMemory(t, config, messaging, prompt, groups, external, nil)
}

func setupMemory(t *testing.T, config Config, messaging Messaging, prompt domain.PromptAssembler, groups func(context.Context, []string) (map[string][]string, error), external domain.ExternalTools, memory domain.MemoryStore) *fixture {
	t.Helper()
	m := &fakeMessaging{in: make(chan domain.InboundEvent), out: make(chan domain.SentMessage, 10)}
	messaging.Name, messaging.Adapter = "test", m
	if groups != nil {
		messaging.Adapter = &groupMessaging{fakeMessaging: m, groups: groups}
	}
	f := setupInstances(t, config, []Messaging{messaging}, prompt, external, memory)
	f.messaging = m
	return f
}

func setupInstances(t *testing.T, config Config, messaging []Messaging, prompt domain.PromptAssembler, external domain.ExternalTools, memory domain.MemoryStore) *fixture {
	t.Helper()
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "history.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	f := &fixture{model: &fakeModel{opened: make(chan *fakeSession, 10)},
		hub: &fakeHub{tools: make(map[string]domain.Tool), sessions: make(map[domain.SessionKey]bool), closed: make(chan domain.SessionKey, 10)}, events: repository.NewEventRepository(db), sessions: repository.NewSessionRepository(db), done: make(chan error, 1)}
	f.core, err = New(config, messaging, f.model, f.hub, f.events, f.sessions, prompt, repository.NewScheduleRepository(db), repository.NewWatchRepository(db), external, memory, repository.NewInboxRepository(db))
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
	return domain.InboundEvent{Kind: domain.InboundEventMessage, Conversation: to, MessageRef: to + "-message", Sender: domain.Sender{ID: user, Kind: domain.SenderHuman}, Text: text, ReceivedAt: time.Now()}
}

func TestConversationFlow(t *testing.T) {
	var prompts atomic.Int32
	f := setup(t, Config{}, Messaging{Owner: "owner", Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Users: []string{"owner", "member"}}, "test:dm": {Users: []string{"owner"}}}},
		func(_ context.Context, identity domain.ToolSession) (string, error) {
			prompts.Add(1)
			return "fresh context", nil
		})
	f.messaging.in <- message("test:room", "owner", "first")
	s := take(t, f.model.opened)
	require.NotContains(t, take(t, s.inputs).Text, "fresh context")
	require.Contains(t, s.config.SystemPrompt, "fresh context")
	require.True(t, f.hub.active("test:room"))
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnStarted}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.messaging.in <- message("test:room", "member", "steer")
	require.Contains(t, take(t, s.inputs).Text, "steer")
	require.EqualValues(t, 1, prompts.Load())
	select {
	case <-f.model.opened:
		t.Fatal("opened another model")
	default:
	}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventText, Text: "never post this"}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventBackgroundDone, Text: "background done"}
	ctx := domain.WithToolSession(t.Context(), domain.ToolSession{Key: "test:room"})
	result, err := f.hub.tools["reply"].Handler(ctx, json.RawMessage(`{"target":"test:room","text":"visible"}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"sent"}`, string(result))
	require.Equal(t, "visible", take(t, f.messaging.out).Message.Text)
	sends, err := f.events.ListOutbound(t.Context(), "test:room", 30)
	require.NoError(t, err)
	require.Len(t, sends, 1)
	f.messaging.in <- message("test:room", "denied", "still recorded")
	f.messaging.in <- message("test:dm", "owner", "private")
	dm := take(t, f.model.opened)
	take(t, dm.inputs)
	require.True(t, f.hub.active("test:dm"))
	inbound, err := f.events.ListInbound(t.Context(), "test:room", 30)
	require.NoError(t, err)
	require.Len(t, inbound, 3)
	require.NoError(t, s.Close())
	require.Equal(t, domain.SessionKey("test:room"), take(t, f.hub.closed))
	records, err := f.events.ListModel(t.Context(), "test:room", 30)
	require.NoError(t, err)
	require.Len(t, records, 5)
	require.Equal(t, domain.ModelEventBackgroundDone, records[0].Event.Kind)
	select {
	case posted := <-f.messaging.out:
		t.Fatalf("unexpected post: %+v", posted)
	default:
	}
}

func TestChannelThreadsIdleAndResume(t *testing.T) {
	f := setup(t, Config{IdleTimeout: 30 * time.Millisecond}, Messaging{Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Users: []string{"member"}}}}, func(_ context.Context, _ domain.ToolSession) (string, error) {
		return "memory context", nil
	})
	require.NoError(t, f.events.SaveInbound(t.Context(), message("test:room", "denied", "old parent history")))
	e := message("test:thread-a", "member", "first")
	e.Parent = "test:room"
	f.messaging.in <- e
	s := take(t, f.model.opened)
	input := take(t, s.inputs)
	require.Contains(t, input.Text, "test:room · thread | member |")
	require.Contains(t, input.Text, "reply → test:thread-a | work → test:thread-a")
	require.NotContains(t, input.Text, "old parent history")
	require.NotContains(t, input.Text, "memory context")
	require.Contains(t, s.config.SystemPrompt, "memory context")
	for _, ref := range []domain.ConversationRef{"test:thread-b", "test:room"} {
		e = message(ref, "member", "queued")
		if ref != "test:room" {
			e.Parent = "test:room"
		}
		f.messaging.in <- e
		input = take(t, s.inputs)
		require.Contains(t, input.Text, "test:room · ")
		if ref == "test:room" {
			require.Contains(t, input.Text, "top level")
		}
	}
	select {
	case <-f.model.opened:
		t.Fatal("opened a session per thread")
	default:
	}
	ctx := domain.WithToolSession(t.Context(), domain.ToolSession{Key: "test:room"})
	_, err := f.hub.tools["reply"].Handler(ctx, json.RawMessage(`{"text":"missing target"}`))
	require.ErrorContains(t, err, "target is required")
	for _, target := range []string{"test:thread-a", "test:thread-b", "test:room", "test:room-message"} {
		_, err = f.hub.tools["reply"].Handler(ctx, json.RawMessage(fmt.Sprintf(`{"target":%q,"text":"answer"}`, target)))
		require.NoError(t, err)
		require.Equal(t, domain.ConversationRef(target), take(t, f.messaging.out).Conversation)
	}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	// A completed turn can still own background work and its tool grant.
	select {
	case <-s.closed:
		t.Fatal("closed a busy session")
	case <-time.After(80 * time.Millisecond):
	}
	require.True(t, f.hub.active("test:room"))
	s.idle.Store(true)
	s.events <- domain.ModelEvent{Kind: domain.ModelEventBackgroundDone, Text: "done"}
	require.Equal(t, domain.SessionKey("test:room"), take(t, f.hub.closed))
	saved, err := f.sessions.Get(t.Context(), "test:room")
	require.NoError(t, err)
	require.Equal(t, "resume-test:room", saved.ResumeToken)
	f.messaging.in <- e
	resumed := take(t, f.model.opened)
	require.Equal(t, "resume-test:room", resumed.config.ResumeToken)
	require.NotEqual(t, s.config.ToolHub.BearerToken, resumed.config.ToolHub.BearerToken)
	require.NotContains(t, take(t, resumed.inputs).Text, "parent_history")
}

func TestOpenFailureRevokesGrant(t *testing.T) {
	f := setup(t, Config{}, Messaging{Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Users: []string{"member"}}}}, nil)
	f.model.fail = true

	f.messaging.in <- message("test:room", "member", "hello")
	require.Equal(t, domain.SessionKey("test:room"), take(t, f.hub.closed))
	require.False(t, f.hub.active("test:room"))
	events, err := f.events.ListInbound(t.Context(), "test:room", 1)
	require.NoError(t, err)
	require.Len(t, events, 1)
}

func TestAcceptRules(t *testing.T) {
	h := &fakeHub{tools: make(map[string]domain.Tool)}
	m := &fakeMessaging{}
	config := Messaging{Owner: "owner", Trust: domain.AcceptRule{Users: []string{"friend"}, Groups: []string{"team"}, Bots: []string{"bot"}},
		UserGroups: map[string][]string{"team": {"member", "group-bot"}}, Rules: map[domain.ConversationRef]domain.AcceptRule{
			"test:room": {Groups: []string{"team"}, RequireMention: true}, "test:bots": {Bots: []string{"bot"}},
			"test:empty": {}, "test:blocked": {Ignore: true}}}
	config.Name, config.Adapter = "test", m
	c, err := New(Config{}, []Messaging{config}, &fakeModel{}, h, &unusedEvents{}, &unusedSessions{}, nil, &unusedSchedules{}, &unusedWatches{}, nil, nil, &unusedInbox{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Shutdown(context.Background())) })
	impl := c.(*core)
	for _, tc := range []struct {
		to                 domain.ConversationRef
		user               string
		bot, mention, want bool
	}{
		{"test:room", "member", false, true, true}, {"test:room", "member", false, false, false}, {"test:room", "outsider", false, true, false},
		{"test:room", "member", true, true, false}, {"test:bots", "bot", true, false, true},
		{"test:bots", "bot", false, false, false}, {"test:blocked", "owner", false, false, false},
		{"test:blocked", "friend", false, true, false}, {"test:blocked", "bot", true, true, false},
		{"test:unknown", "owner", false, false, true}, {"test:unknown", "friend", false, false, true},
		{"test:unknown", "member", false, false, true}, {"test:unknown", "outsider", false, true, false},
		{"test:unknown", "bot", true, false, true}, {"test:unknown", "friend", true, false, false},
		{"test:unknown", "group-bot", true, false, false}, {"test:unknown", "owner", true, false, false},
		{"test:room", "friend", false, true, false}, {"test:room", "owner", false, true, true},
		{"test:room", "owner", false, false, false}, {"test:empty", "owner", false, false, true},
		{"test:empty", "friend", false, true, false}, {"test:empty", "bot", true, true, false},
	} {
		for _, direct := range []bool{false, true} {
			e := message(tc.to, tc.user, "")
			e.MentionsMe, e.Direct = tc.mention, direct
			if tc.bot {
				e.Sender.Kind = domain.SenderBot
			}
			_, ok := impl.accepted(e)
			require.Equal(t, tc.want, ok, "%+v direct=%v", tc, direct)
		}
	}
	config.Trust.Users[0] = "mutated"
	config.Trust.Bots[0] = "mutated-bot"
	config.Rules["test:empty"] = domain.AcceptRule{Users: []string{"member"}}
	r, ok := impl.accepted(message("test:unknown", "friend", ""))
	require.True(t, ok)
	require.Contains(t, r.users, "owner")
	require.Contains(t, r.bots, "bot")
	thread := message("test:thread", "member", "")
	thread.Parent = "test:room"
	thread.MentionsMe = true
	_, ok = impl.accepted(thread)
	require.True(t, ok)
	require.Equal(t, domain.ConversationRef("test:room"), channel(thread))
	thread.Parent = "test:blocked"
	_, ok = impl.accepted(thread)
	require.False(t, ok)
}

func TestMissingEntryTrustAcceptsOnlyOwner(t *testing.T) {
	instance, err := newMessagingInstance(Messaging{Owner: "owner"})
	require.NoError(t, err)
	impl := &core{messaging: &messagingRouter{byName: map[string]*messagingInstance{"test": instance}}}
	for _, direct := range []bool{false, true} {
		for _, sender := range []string{"owner", "other"} {
			event := message("test:unlisted", sender, "")
			event.Direct = direct
			_, accepted := impl.accepted(event)
			require.Equal(t, sender == "owner", accepted)
		}
	}
}

type unusedEvents struct{ domain.EventRepository }
type unusedSessions struct{ domain.SessionRepository }
type unusedSchedules struct{ domain.ScheduleRepository }
type unusedWatches struct{ domain.WatchRepository }

func (*fakeMessaging) Shutdown(context.Context) error { return nil }

func TestShutdownDuringInput(t *testing.T) {
	f := setup(t, Config{}, Messaging{Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Users: []string{"member"}}}}, nil)
	entered := make(chan struct{})
	f.model.send = func(ctx context.Context, _ domain.UserInput) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	f.messaging.in <- message("test:room", "member", "start")
	take(t, entered)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, f.core.Shutdown(ctx))
}

type unusedInbox struct{ domain.InboxRepository }

func TestConversationCanReplyToOwner(t *testing.T) {
	f := setup(t, Config{}, Messaging{Owner: "owner", Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Users: []string{"member"}}}}, nil)
	f.messaging.in <- message("test:room", "member", "report to owner")
	s := take(t, f.model.opened)
	take(t, s.inputs)
	ctx := domain.WithToolSession(t.Context(), domain.ToolSession{Key: "test:room"})
	_, err := f.hub.tools["reply"].Handler(ctx, json.RawMessage(`{"target":"owner","text":"requested report"}`))
	require.NoError(t, err)
	sent := take(t, f.messaging.out)
	require.Equal(t, domain.ConversationRef("test:dm:owner"), sent.Conversation)
	require.Equal(t, "requested report", sent.Message.Text)
	history, err := f.events.ListOutbound(t.Context(), "test:dm:owner", 10)
	require.NoError(t, err)
	require.Len(t, history, 1)
}
