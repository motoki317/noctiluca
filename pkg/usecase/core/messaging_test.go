package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/stretchr/testify/require"
)

type sourceCall struct {
	ref    domain.ConversationRef
	sender string
}

type namedMessaging struct {
	*fakeMessaging
	name        string
	filter      func(domain.ConversationRef) bool
	info        chan sourceCall
	dmUsers     chan string
	start       func(context.Context, func(context.Context, domain.InboundEvent) error) error
	stops       atomic.Int32
	shutdownErr error
}

func newNamedMessaging(name string) *namedMessaging {
	return &namedMessaging{fakeMessaging: &fakeMessaging{in: make(chan domain.InboundEvent), out: make(chan domain.SentMessage, 10)},
		name: name, info: make(chan sourceCall, 20), dmUsers: make(chan string, 10)}
}

func (m *namedMessaging) Start(ctx context.Context, consume func(context.Context, domain.InboundEvent) error) error {
	if m.start != nil {
		return m.start(ctx, consume)
	}
	return m.fakeMessaging.Start(ctx, consume)
}

func (m *namedMessaging) Shutdown(context.Context) error {
	m.stops.Add(1)
	return m.shutdownErr
}

func (m *namedMessaging) DirectMessage(_ context.Context, user string) (domain.ConversationRef, error) {
	m.dmUsers <- user
	return domain.ConversationRef(m.name + ":dm:" + user), nil
}

func (m *namedMessaging) SourceInfo(_ context.Context, ref domain.ConversationRef, sender string, _ bool) domain.SourceInfo {
	m.info <- sourceCall{ref: ref, sender: sender}
	return domain.SourceInfo{Conversation: m.name + " room", Sender: m.name + " user"}
}

func (m *namedMessaging) SetConversationFilter(filter func(domain.ConversationRef) bool) {
	m.filter = filter
}

func (m *namedMessaging) ToolNotes() string {
	return "Use " + m.name + "_send with " + m.name + ": refs."
}
func (m *namedMessaging) Tools() []domain.Tool {
	return []domain.Tool{{Name: m.name + "_send", Description: "Send a message.", InputSchema: json.RawMessage(`{"type":"object"}`)}}
}

func TestMessagingRouter(t *testing.T) {
	alpha, beta := newNamedMessaging("alpha"), newNamedMessaging("beta")
	router, err := newMessagingRouter([]Messaging{{Name: "beta", Adapter: beta}, {Name: "alpha", Adapter: alpha}})
	require.NoError(t, err)
	require.Equal(t, "beta", router.instances[0].config.Name)
	for _, adapter := range []*namedMessaging{alpha, beta} {
		ref := domain.ConversationRef(adapter.name + ":room:thread:opaque")
		id, err := router.Send(t.Context(), ref, domain.OutboundMessage{Text: "hello"})
		require.NoError(t, err)
		require.Equal(t, "sent", id)
		require.Equal(t, ref, take(t, adapter.out).Conversation)
	}
	for _, ref := range []domain.ConversationRef{"", "alpha", "missing:room", ":room"} {
		_, err := router.Send(t.Context(), ref, domain.OutboundMessage{})
		require.ErrorContains(t, err, "unknown messaging entry")
	}
	alpha.shutdownErr = errors.New("shutdown failed")
	require.ErrorIs(t, router.Shutdown(t.Context()), alpha.shutdownErr)
	require.EqualValues(t, 1, alpha.stops.Load())
	require.EqualValues(t, 1, beta.stops.Load())
}

func TestMessagingConfigurationValidation(t *testing.T) {
	m := &fakeMessaging{}
	for _, tc := range []struct {
		name      string
		instances []Messaging
		want      string
	}{
		{"empty", nil, "at least one"},
		{"missing name", []Messaging{{Adapter: m}}, "invalid messaging entry name"},
		{"invalid name", []Messaging{{Name: "a:b", Adapter: m}}, "invalid messaging entry name"},
		{"nil adapter", []Messaging{{Name: "alpha"}}, "requires an adapter"},
		{"duplicate", []Messaging{{Name: "alpha", Adapter: m}, {Name: "alpha", Adapter: m}}, "duplicate messaging entry"},
		{"unknown rule", []Messaging{{Name: "alpha", Adapter: m, Rules: map[domain.ConversationRef]domain.AcceptRule{"other:room": {}}}}, "unknown messaging entry"},
		{"foreign rule", []Messaging{{Name: "alpha", Adapter: m, Rules: map[domain.ConversationRef]domain.AcceptRule{"beta:room": {}}}, {Name: "beta", Adapter: m}}, "does not belong"},
		{"negative refresh", []Messaging{{Name: "alpha", Adapter: m, GroupRefreshInterval: -time.Second}}, "group refresh interval"},
		{"unknown group", []Messaging{{Name: "alpha", Adapter: m, Trust: domain.AcceptRule{Groups: []string{"missing"}}}}, "unknown user group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newMessagingRouter(tc.instances)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestConversationFilterSkipsOnlyIgnoredConversations(t *testing.T) {
	adapter := newNamedMessaging("test")
	c, err := New(Config{}, []Messaging{{Name: "test", Adapter: adapter,
		Trust: domain.AcceptRule{Bots: []string{"bot"}}, Rules: map[domain.ConversationRef]domain.AcceptRule{
			"test:ignored": {Ignore: true}, "test:empty": {},
		},
	}}, &fakeModel{}, &fakeHub{tools: make(map[string]domain.Tool)}, &unusedEvents{}, &unusedSessions{}, nil, &unusedSchedules{}, &unusedWatches{}, nil, nil, &unusedInbox{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Shutdown(t.Context())) })
	for _, tc := range []struct {
		ref  domain.ConversationRef
		want bool
	}{{"test:unlisted", true}, {"test:empty", true}, {"test:ignored", false}, {"other:room", false}} {
		require.Equal(t, tc.want, adapter.filter(tc.ref), "%s", tc.ref)
	}
	require.True(t, c.(*core).acceptSession(domain.SessionContext{Conversation: "test:unlisted"}))
	require.False(t, c.(*core).acceptSession(domain.SessionContext{Conversation: "test:ignored"}))
}

func TestMultipleMessagingRoutesInputsRepliesAndOwner(t *testing.T) {
	alpha, beta := newNamedMessaging("alpha"), newNamedMessaging("beta")
	f := setupInstances(t, Config{}, []Messaging{
		{Name: "beta", Adapter: beta, Owner: "primary", Rules: map[domain.ConversationRef]domain.AcceptRule{"beta:room": {Users: []string{"primary"}}}},
		{Name: "alpha", Adapter: alpha, Owner: "secondary", Rules: map[domain.ConversationRef]domain.AcceptRule{"alpha:room": {Users: []string{"primary", "secondary"}}}},
	}, nil, nil, nil)
	impl := f.core.(*core)
	beta.in <- message("beta:room", "primary", "first owner")
	first := take(t, f.model.opened)
	require.Contains(t, take(t, first.inputs).Text, "beta user (owner)")
	require.Equal(t, sourceCall{"beta:room", "primary"}, take(t, beta.info))
	alpha.in <- message("alpha:room", "primary", "same ID on another messaging entry")
	second := take(t, f.model.opened)
	require.NotContains(t, take(t, second.inputs).Text, "(owner)")
	require.Equal(t, sourceCall{"alpha:room", "primary"}, take(t, alpha.info))
	alpha.in <- message("alpha:room", "secondary", "second owner")
	require.Contains(t, take(t, second.inputs).Text, "alpha user (owner)")
	take(t, alpha.info)
	for _, sentence := range []string{
		"One session covers a channel and all its threads.", "Post only when addressed or reporting requested work.",
		"Reply only when addressed (a mention, a request to you, or a DM). Stay silent for chatter between other people.",
		"Copy refs exactly.", "Give watch and wake_at the ref where the result should be posted.", "Do not wait or poll.",
		"run anything that may take over a minute, such as a build, a test suite, or a subagent, with your background tools or watch, and end the turn immediately.",
		"Reply attachments must come from the same messaging entry as the ref you post to (its first segment).",
		"alpha:\n" + alpha.ToolNotes(), "beta:\n" + beta.ToolNotes(),
	} {
		require.Equal(t, 1, strings.Count(second.config.SystemPrompt, sentence), sentence)
	}
	require.Contains(t, f.hub.sources, "messaging:alpha")
	require.Contains(t, f.hub.sources, "messaging:beta")
	require.Contains(t, f.hub.tools, "alpha_send")
	require.Contains(t, f.hub.tools, "beta_send")
	require.True(t, alpha.filter("alpha:room"))
	require.False(t, alpha.filter("beta:room"))
	require.False(t, beta.filter("alpha:room"))
	require.False(t, beta.filter("missing:room"))
	ctx := domain.WithToolSession(t.Context(), domain.ToolSession{Key: "alpha:room"})
	for _, adapter := range []*namedMessaging{alpha, beta} {
		ref := domain.ConversationRef(adapter.name + ":room:thread")
		raw, err := json.Marshal(map[string]any{"target": ref, "text": "reply", "attachments": []domain.Attachment{{Ref: "file"}}})
		require.NoError(t, err)
		_, err = f.hub.tools["reply"].Handler(ctx, raw)
		require.NoError(t, err)
		sent := take(t, adapter.out)
		require.Equal(t, ref, sent.Conversation)
		require.Equal(t, []domain.Attachment{{Ref: "file"}}, sent.Message.Attachments)
		history, err := f.events.ListOutbound(t.Context(), ref, 10)
		require.NoError(t, err)
		require.Len(t, history, 1)
	}
	_, err := f.hub.tools["reply"].Handler(ctx, json.RawMessage(`{"target":"missing:room","text":"no route"}`))
	require.ErrorContains(t, err, "unknown messaging entry")
	_, err = impl.prepare(t.Context(), domain.InboxItem{SessionKey: "alpha:room", ReadyAt: time.Now(), Payload: domain.InboxPayload{Target: "missing:room"}})
	require.ErrorContains(t, err, "unknown messaging entry")
	_, err = f.hub.tools["reply"].Handler(ctx, json.RawMessage(`{"target":"owner","text":"owner reply"}`))
	require.NoError(t, err)
	require.Equal(t, "primary", take(t, beta.dmUsers))
	require.Equal(t, domain.ConversationRef("beta:dm:primary"), take(t, beta.out).Conversation)
	require.NoError(t, impl.notifyOwner(t.Context(), "owner notice"))
	require.Equal(t, "primary", take(t, beta.dmUsers))
	require.Equal(t, "owner notice", take(t, beta.out).Message.Text)
	history, err := f.events.ListOutbound(t.Context(), "beta:dm:primary", 10)
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.Empty(t, alpha.out)
	require.Empty(t, alpha.dmUsers)
	require.NoError(t, f.core.Shutdown(t.Context()))
	require.EqualValues(t, 1, alpha.stops.Load())
	require.EqualValues(t, 1, beta.stops.Load())
}

func TestMessagingRetriesDoNotStopOtherInstances(t *testing.T) {
	broken, healthy := newNamedMessaging("broken"), newNamedMessaging("healthy")
	attempts := make(chan struct{}, 3)
	broken.start = func(context.Context, func(context.Context, domain.InboundEvent) error) error {
		attempts <- struct{}{}
		return errors.New("connection lost")
	}
	f := setupInstances(t, Config{}, []Messaging{{Name: "broken", Adapter: broken},
		{Name: "healthy", Adapter: healthy, Rules: map[domain.ConversationRef]domain.AcceptRule{"healthy:room": {Users: []string{"member"}}}},
	}, nil, nil, nil)
	take(t, attempts)
	healthy.in <- message("healthy:room", "member", "during outage")
	session := take(t, f.model.opened)
	require.Contains(t, take(t, session.inputs).Text, "during outage")
	take(t, attempts)
	healthy.in <- message("healthy:room", "member", "after retry")
	require.Contains(t, take(t, session.inputs).Text, "after retry")
	require.NoError(t, f.core.Shutdown(t.Context()))
	require.EqualValues(t, 1, broken.stops.Load())
	require.EqualValues(t, 1, healthy.stops.Load())
}

func TestMessagingPersistenceFailureStopsAllInstances(t *testing.T) {
	broken, healthy := newNamedMessaging("broken"), newNamedMessaging("healthy")
	fail := make(chan struct{})
	broken.start = func(ctx context.Context, _ func(context.Context, domain.InboundEvent) error) error {
		select {
		case <-fail:
			return domain.ErrPersistence
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f := setupInstances(t, Config{}, []Messaging{{Name: "broken", Adapter: broken},
		{Name: "healthy", Adapter: healthy, Rules: map[domain.ConversationRef]domain.AcceptRule{"healthy:room": {Users: []string{"member"}}}},
	}, nil, nil, nil)
	f.wantError = domain.ErrPersistence.Error()
	healthy.in <- message("healthy:room", "member", "active before failure")
	session := take(t, f.model.opened)
	take(t, session.inputs)
	close(fail)
	take(t, session.closed)
	take(t, f.core.(*core).done)
	require.EqualValues(t, 1, broken.stops.Load())
	require.EqualValues(t, 1, healthy.stops.Load())
}

type namedGroupMessaging struct {
	*namedMessaging
	groups func(context.Context, []string) (map[string][]string, error)
}

func (m *namedGroupMessaging) UserGroups(ctx context.Context, ids []string) (map[string][]string, error) {
	return m.groups(ctx, ids)
}

func TestMessagingGroupsAndDirectRulesAreIndependent(t *testing.T) {
	alpha, beta := newNamedMessaging("alpha"), newNamedMessaging("beta")
	instances := make([]Messaging, 0, 2)
	responses := make(map[string]chan map[string][]string)
	calls := make(map[string]chan []string)
	for _, adapter := range []*namedMessaging{alpha, beta} {
		name := adapter.name
		response, call := make(chan map[string][]string, 2), make(chan []string, 3)
		responses[name], calls[name] = response, call
		response <- map[string][]string{"team": {name}, name: {name + "-dm"}}
		provider := &namedGroupMessaging{namedMessaging: adapter, groups: func(ctx context.Context, ids []string) (map[string][]string, error) {
			select {
			case call <- ids:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			select {
			case groups := <-response:
				return groups, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
		instances = append(instances, Messaging{Name: name, Adapter: provider, GroupRefreshInterval: 10 * time.Millisecond,
			Rules: map[domain.ConversationRef]domain.AcceptRule{domain.ConversationRef(name + ":room"): {Groups: []string{"team"}}, domain.ConversationRef(name + ":blocked"): {Ignore: true}},
			Trust: domain.AcceptRule{Groups: []string{name}},
		})
	}
	f := setupInstances(t, Config{}, instances, nil, nil, nil)
	for _, adapter := range []*namedMessaging{alpha, beta} {
		name := adapter.name
		require.Equal(t, []string{name, "team"}, take(t, calls[name]))
		adapter.in <- message(domain.ConversationRef(name+":room"), "outsider", "denied")
		adapter.in <- message(domain.ConversationRef(name+":room"), name, "own group")
		session := take(t, f.model.opened)
		require.Contains(t, take(t, session.inputs).Text, "own group")
		require.True(t, adapter.filter(domain.ConversationRef(name+":room")))
		require.False(t, adapter.filter(domain.ConversationRef(name+":blocked")))
		wrong := message(domain.ConversationRef(name+":dm"), "outsider", "wrong DM user")
		wrong.Direct = true
		adapter.in <- wrong
		direct := message(domain.ConversationRef(name+":dm"), name+"-dm", "own DM rule")
		direct.Direct = true
		adapter.in <- direct
		dm := take(t, f.model.opened)
		require.Contains(t, take(t, dm.inputs).Text, "own DM rule")
		require.Equal(t, domain.SessionKey(name+":dm"), dm.config.Key)
		take(t, calls[name])
	}
	responses["alpha"] <- map[string][]string{"team": {}, "alpha": {"alpha-dm"}}
	take(t, calls["alpha"])
	require.True(t, alpha.filter("alpha:room"))
	require.True(t, alpha.filter("alpha:dm"))
	require.True(t, beta.filter("beta:room"))
	require.True(t, f.hub.active("beta:room"))
	require.True(t, f.hub.active("beta:dm"))
	impl := f.core.(*core)
	impl.mu.Lock()
	_, crossAlpha := impl.accepted(message("alpha:room", "beta", ""))
	_, crossBeta := impl.accepted(message("beta:room", "alpha", ""))
	impl.mu.Unlock()
	require.False(t, crossAlpha)
	require.False(t, crossBeta)
}
