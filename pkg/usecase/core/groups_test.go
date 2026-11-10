package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type groupMessaging struct {
	*fakeMessaging
	groups func(context.Context, []string) (map[string][]string, error)
}

func (m *groupMessaging) UserGroups(ctx context.Context, ids []string) (map[string][]string, error) {
	return m.groups(ctx, ids)
}

func TestGroupRefreshKeepsSessionAndAcceptsNewUsers(t *testing.T) {
	type response struct {
		members []string
		err     error
	}
	responses := make(chan response, 3)
	responses <- response{members: []string{"owner"}}
	calls := make(chan []string, 5)
	provider := func(ctx context.Context, ids []string) (map[string][]string, error) {
		calls <- ids
		select {
		case value := <-responses:
			return map[string][]string{"group": value.members}, value.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := setupGroups(t, Config{}, Messaging{Owner: "owner", GroupRefreshInterval: 10 * time.Millisecond, Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Groups: []string{"group"}}}}, nil, provider)
	require.Equal(t, []string{"group"}, take(t, calls))
	e := message("test:thread", "owner", "first")
	e.Parent = "test:room"
	f.messaging.in <- e
	s := take(t, f.model.opened)
	take(t, s.inputs)
	require.True(t, f.hub.active("test:room"))
	take(t, calls)
	responses <- response{err: errors.New("temporary outage")}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.messaging.in <- e
	take(t, s.inputs)
	require.True(t, f.hub.active("test:room"))
	take(t, calls)
	responses <- response{members: []string{"owner", "member"}}
	take(t, calls)
	e.Sender.ID = "member"
	f.messaging.in <- e
	take(t, s.inputs)
	require.True(t, f.hub.active("test:room"))
	select {
	case <-f.hub.closed:
		t.Fatal("closed a session after membership expanded")
	default:
	}
}

func TestGroupUpdatesKeepSessionAndRejectRemovedUsers(t *testing.T) {
	f := setup(t, Config{}, Messaging{Owner: "owner", UserGroups: map[string][]string{"group": {"owner", "member"}}, Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Groups: []string{"group"}}}}, nil)
	f.messaging.in <- message("test:room", "member", "first")
	s := take(t, f.model.opened)
	take(t, s.inputs)
	require.NoError(t, f.core.(*core).applyGroups(f.core.(*core).messaging.instances[0], map[string][]string{"group": {"owner"}}))
	require.Error(t, f.core.(*core).applyGroups(f.core.(*core).messaging.instances[0], map[string][]string{}))
	f.messaging.in <- message("test:room", "member", "denied after refresh")
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.messaging.in <- message("test:room", "owner", "still accepted")
	require.Contains(t, take(t, s.inputs).Text, "still accepted")
	require.True(t, f.hub.active("test:room"))
	select {
	case <-f.model.opened:
		t.Fatal("replaced the live session")
	default:
	}
	events, err := f.events.ListInbound(t.Context(), "test:room", 10)
	require.NoError(t, err)
	require.Len(t, events, 3)
}

func TestGroupStartupFailure(t *testing.T) {
	called := make(chan struct{})
	f := setupGroups(t, Config{}, Messaging{Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Groups: []string{"group"}}}}, nil,
		func(context.Context, []string) (map[string][]string, error) {
			select {
			case <-called:
			default:
				close(called)
			}
			return nil, errors.New("group lookup failed")
		})
	_ = f
	take(t, called)
}

func TestEntryTrustRefreshAndRestore(t *testing.T) {
	f := setup(t, Config{}, Messaging{Owner: "owner", Trust: domain.AcceptRule{Groups: []string{"team"}}, UserGroups: map[string][]string{"team": {"owner"}}, Rules: map[domain.ConversationRef]domain.AcceptRule{"test:blocked-direct": {Ignore: true}}}, nil)
	blocked := message("test:blocked-direct", "owner", "denied override")
	blocked.Direct = true
	f.messaging.in <- blocked
	e := message("test:direct-thread", "owner", "first")
	e.Parent = "test:direct"
	e.Direct = true
	f.messaging.in <- e
	s := take(t, f.model.opened)
	require.Contains(t, take(t, s.inputs).Text, "first")
	require.True(t, f.hub.active("test:direct"))
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	require.Eventually(t, func() bool {
		records, err := f.events.ListModel(t.Context(), "test:direct", 1)
		return err == nil && len(records) == 1 && records[0].Event.Kind == domain.ModelEventTurnFinished
	}, time.Second, time.Millisecond)
	require.NoError(t, f.core.(*core).applyGroups(f.core.(*core).messaging.instances[0], map[string][]string{"team": {"owner", "member"}}))
	e.Sender.ID = "member"
	e.Text = "accepted without restart"
	f.messaging.in <- e
	require.Contains(t, take(t, s.inputs).Text, "accepted without restart")
	require.NoError(t, s.Close())
	require.Equal(t, domain.SessionKey("test:direct"), take(t, f.hub.closed))
	item, err := f.core.(*core).workItem(t.Context(), "test:direct")
	require.NoError(t, err)
	item.SourceKey = "wake-test"
	item.ReadyAt = time.Now()
	item.Payload.Input.Text = "wake after close"
	resumed := take(t, f.model.opened)
	require.Equal(t, "resume-test:direct", resumed.config.ResumeToken)
	require.Contains(t, take(t, resumed.inputs).Text, "previous turn was interrupted: model process exited")
	_, err = f.core.(*core).inbox.Enqueue(t.Context(), item)
	require.NoError(t, err)
	f.core.(*core).reconciler.Notify()
	require.Contains(t, take(t, resumed.inputs).Text, "wake after close")
	require.True(t, f.hub.active("test:direct"))
	resumed.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	e.Sender.ID = "member"
	e.Text = "accepted member"
	f.messaging.in <- e
	require.Contains(t, take(t, resumed.inputs).Text, "accepted member")
	select {
	case <-f.model.opened:
		t.Fatal("unexpected session")
	default:
	}
}

func TestIgnoredConversationRetiresSessionAndRejectsWake(t *testing.T) {
	f := setup(t, Config{}, Messaging{Owner: "owner", UserGroups: map[string][]string{"group": {"member"}}, Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Groups: []string{"group"}}}}, nil)
	f.messaging.in <- message("test:room", "member", "first")
	s := take(t, f.model.opened)
	take(t, s.inputs)
	item, err := f.core.(*core).workItem(t.Context(), "test:room")
	require.NoError(t, err)
	impl := f.core.(*core)
	impl.mu.Lock()
	instance := impl.messaging.instances[0]
	instance.config.Rules["test:room"] = domain.AcceptRule{Ignore: true}
	groups := instance.config.UserGroups
	impl.mu.Unlock()
	require.NoError(t, impl.applyGroups(instance, groups))
	require.Equal(t, domain.SessionKey("test:room"), take(t, f.hub.closed))
	item.SourceKey = "wake:removed"
	item.ReadyAt = time.Now()
	item.Payload.Input.Text = "unaccepted wake"
	_, err = f.core.(*core).inbox.Enqueue(t.Context(), item)
	require.NoError(t, err)
	f.core.(*core).reconciler.Notify()
	require.Eventually(t, func() bool {
		items, err := f.core.(*core).inbox.Active(t.Context())
		return err == nil && len(items) == 0
	}, time.Second, time.Millisecond)
	record, err := f.sessions.Get(t.Context(), "test:room")
	require.NoError(t, err)
	require.Equal(t, "resume-test:room", record.ResumeToken)
	select {
	case <-f.model.opened:
		t.Fatal("opened a model without an accept rule")
	default:
	}
}
