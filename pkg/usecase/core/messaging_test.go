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

func (m *namedMessaging) SourceInfo(_ context.Context, event domain.InboundEvent, _ bool) domain.SourceInfo {
	m.info <- sourceCall{ref: event.Conversation, sender: event.Sender.ID}
	return domain.SourceInfo{Conversation: m.name + " room", Sender: m.name + " user"}
}

func (m *namedMessaging) SetConversationFilter(filter func(domain.ConversationRef) bool) {
	m.filter = filter
}

func (m *namedMessaging) ToolNotes(context.Context) string {
	return "Use " + m.name + "_send with native IDs."
}
func (m *namedMessaging) Tools() []domain.Tool {
	return []domain.Tool{{Name: m.name + "_send", Description: "Send a message.", InputSchema: json.RawMessage(`{"type":"object"}`)}}
}

func TestMessagingRouter(t *testing.T) {
	alpha, beta := newNamedMessaging("alpha"), newNamedMessaging("beta")
	router, err := newMessagingRouter([]Messaging{{Name: "beta", Adapter: beta}, {Name: "alpha", Adapter: alpha}})
	require.NoError(t, err)
	require.Equal(t, "beta", router.instances[0].config.Name)
	for _, ref := range []domain.ConversationRef{"alpha", "missing:room"} {
		_, err := router.resolve(ref)
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

func TestMultipleMessagingRoutesInputsAndOwner(t *testing.T) {
	alpha, beta := newNamedMessaging("alpha"), newNamedMessaging("beta")
	f := setup(t, Config{}, []Messaging{
		{Name: "beta", Adapter: beta, Owner: "primary", Rules: map[domain.ConversationRef]domain.AcceptRule{"beta:room": {Users: []string{"primary"}}}},
		{Name: "alpha", Adapter: alpha, Owner: "secondary", Rules: map[domain.ConversationRef]domain.AcceptRule{"alpha:room": {Users: []string{"primary", "secondary"}}}},
	}, fixtureOptions{})
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
		"alpha:\nUse alpha_send with native IDs.", "beta:\nUse beta_send with native IDs.",
	} {
		require.Equal(t, 1, strings.Count(second.config.SystemPrompt, sentence), sentence)
	}
	for _, rule := range []string{"Chat is", "work is", "chat at", "work at"} {
		require.NotContains(t, second.config.SystemPrompt, rule)
	}
	require.Contains(t, f.hub.tools, "alpha_send")
	require.Contains(t, f.hub.tools, "beta_send")
	require.True(t, alpha.filter("alpha:room"))
	require.False(t, alpha.filter("beta:room"))
	require.False(t, beta.filter("alpha:room"))
	require.False(t, beta.filter("missing:room"))
	require.NotContains(t, f.hub.tools, "reply")
	require.NoError(t, impl.notifyOwner(t.Context(), "owner notice"))
	require.Equal(t, "primary", take(t, beta.dmUsers))
	require.Equal(t, "owner notice", take(t, beta.out).Message.Text)
	history, err := f.events.ListOutbound(t.Context(), "beta:dm:primary", 10)
	require.NoError(t, err)
	require.Len(t, history, 1)
}

func TestMessagingRetriesDoNotStopOtherInstances(t *testing.T) {
	broken, healthy := newNamedMessaging("broken"), newNamedMessaging("healthy")
	attempts := make(chan struct{}, 3)
	broken.start = func(context.Context, func(context.Context, domain.InboundEvent) error) error {
		attempts <- struct{}{}
		return errors.New("connection lost")
	}
	f := setup(t, Config{}, []Messaging{{Name: "broken", Adapter: broken},
		{Name: "healthy", Adapter: healthy, Rules: map[domain.ConversationRef]domain.AcceptRule{"healthy:room": {Users: []string{"member"}}}},
	}, fixtureOptions{})
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
	f := setup(t, Config{}, []Messaging{{Name: "broken", Adapter: broken},
		{Name: "healthy", Adapter: healthy, Rules: map[domain.ConversationRef]domain.AcceptRule{"healthy:room": {Users: []string{"member"}}}},
	}, fixtureOptions{})
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
	calls := make(map[string]chan []string)
	for _, adapter := range []*namedMessaging{alpha, beta} {
		name := adapter.name
		calls[name] = make(chan []string, 1)
		provider := &namedGroupMessaging{namedMessaging: adapter, groups: func(_ context.Context, ids []string) (map[string][]string, error) {
			calls[name] <- ids
			return map[string][]string{"team": {name}, name: {name + "-dm"}}, nil
		}}
		instances = append(instances, Messaging{Name: name, Adapter: provider,
			Rules: map[domain.ConversationRef]domain.AcceptRule{domain.ConversationRef(name + ":room"): {Groups: []string{"team"}}, domain.ConversationRef(name + ":blocked"): {Ignore: true}, domain.ConversationRef(name + ":empty"): {}},
			Trust: domain.AcceptRule{Groups: []string{name}},
		})
	}
	f := setup(t, Config{}, instances, fixtureOptions{})
	impl := f.core.(*core)
	for _, adapter := range []*namedMessaging{alpha, beta} {
		name := adapter.name
		require.Equal(t, []string{name, "team"}, take(t, calls[name]))
		// Receiving an input means startup applied both group snapshots.
		adapter.in <- message(domain.ConversationRef(name+":room"), "outsider", "denied")
		for _, tc := range []struct {
			room, sender string
			direct, want bool
		}{
			{"room", "outsider", false, false},
			{"room", "alpha", false, name == "alpha"},
			{"room", "beta", false, name == "beta"},
			{"dm", name + "-dm", true, true},
			{"dm", "outsider", true, false},
		} {
			event := message(domain.ConversationRef(name+":"+tc.room), tc.sender, "")
			event.Direct = tc.direct
			_, accepted := impl.accepted(event)
			require.Equal(t, tc.want, accepted, "%s: %+v", name, tc)
		}
		for _, tc := range []struct {
			room string
			want bool
		}{{"room", true}, {"dm", true}, {"empty", true}, {"blocked", false}} {
			ref := domain.ConversationRef(name + ":" + tc.room)
			require.Equal(t, tc.want, adapter.filter(ref), "%s", ref)
			require.Equal(t, tc.want, impl.acceptSession(domain.SessionContext{Conversation: ref}), "%s", ref)
		}
	}
}
