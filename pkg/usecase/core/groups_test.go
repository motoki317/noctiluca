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

func TestGroupRefreshRetiresOwnerAndResumesMember(t *testing.T) {
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
	f := setupGroups(t, Config{Owner: "owner", GroupRefreshInterval: 10 * time.Millisecond, Rules: map[domain.ConversationRef]domain.AcceptRule{"room": {Groups: []string{"group"}}}}, nil, provider)
	require.Equal(t, []string{"group"}, take(t, calls))
	e := message("thread", "owner", "first")
	e.Parent = "room"
	f.messaging.in <- e
	s := take(t, f.model.opened)
	take(t, s.inputs)
	require.Equal(t, domain.ActorOwner, f.hub.actor("thread"))
	take(t, calls)
	responses <- response{err: errors.New("temporary outage")}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.messaging.in <- e
	take(t, s.inputs)
	require.Equal(t, domain.ActorOwner, f.hub.actor("thread"))
	take(t, calls)
	responses <- response{members: []string{"owner", "member"}}
	require.Equal(t, domain.SessionKey("thread"), take(t, f.hub.closed))
	e.Sender.ID = "member"
	f.messaging.in <- e
	resumed := take(t, f.model.opened)
	take(t, resumed.inputs)
	require.Equal(t, "resume-thread", resumed.config.ResumeToken)
	require.Equal(t, domain.ActorMember, f.hub.actor("thread"))
	require.NotEqual(t, s.config.ToolHub.BearerToken, resumed.config.ToolHub.BearerToken)
}

func TestGroupUpdatesKeepMemberActorAndRejectRemovedUsers(t *testing.T) {
	f := setup(t, Config{Owner: "owner", UserGroups: map[string][]string{"group": {"owner", "member"}}, Rules: map[domain.ConversationRef]domain.AcceptRule{"room": {Groups: []string{"group"}}}}, nil)
	f.messaging.in <- message("room", "member", "first")
	s := take(t, f.model.opened)
	take(t, s.inputs)
	require.NoError(t, f.core.UpdateUserGroups(t.Context(), map[string][]string{"group": {"owner"}}))
	require.Error(t, f.core.UpdateUserGroups(t.Context(), map[string][]string{}))
	f.messaging.in <- message("room", "member", "denied after refresh")
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.messaging.in <- message("room", "owner", "still accepted")
	require.Contains(t, take(t, s.inputs).Text, "still accepted")
	require.Equal(t, domain.ActorMember, f.hub.actor("room"))
	select {
	case <-f.model.opened:
		t.Fatal("promoted a live member session")
	default:
	}
	events, err := f.events.ListInbound(t.Context(), "room", 10)
	require.NoError(t, err)
	require.Len(t, events, 3)
}

func TestGroupStartupFailure(t *testing.T) {
	called := make(chan struct{})
	f := setupGroups(t, Config{Rules: map[domain.ConversationRef]domain.AcceptRule{"room": {Groups: []string{"group"}}}}, nil,
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

func (m *fakeMessaging) DirectConversation(ref domain.ConversationRef) bool {
	return ref == "direct" || ref == "direct-thread" || ref == "blocked-direct"
}

func TestDirectDefaultsRefreshAndRestore(t *testing.T) {
	f := setup(t, Config{Owner: "owner", DirectMessages: &domain.AcceptRule{Groups: []string{"team"}}, UserGroups: map[string][]string{"team": {"owner"}}, Rules: map[domain.ConversationRef]domain.AcceptRule{"blocked-direct": {}}}, nil)
	f.messaging.in <- message("blocked-direct", "owner", "denied override")
	f.messaging.in <- message("unknown", "owner", "denied channel")
	e := message("direct-thread", "owner", "first")
	e.Parent = "direct"
	f.messaging.in <- e
	s := take(t, f.model.opened)
	require.Contains(t, take(t, s.inputs).Text, "first")
	require.Equal(t, domain.ActorOwner, f.hub.actor("direct-thread"))
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	require.Eventually(t, func() bool {
		items, err := f.core.(*core).inbox.Active(t.Context())
		return err == nil && len(items) == 0
	}, time.Second, time.Millisecond)
	require.NoError(t, f.core.UpdateUserGroups(t.Context(), map[string][]string{"team": {"owner", "member"}}))
	require.Equal(t, domain.SessionKey("direct-thread"), take(t, f.hub.closed))
	item, err := f.core.(*core).workItem(t.Context(), "direct-thread")
	require.NoError(t, err)
	item.SourceKey = "wake-test"
	item.ReadyAt = time.Now()
	item.Payload.Input.Text = "wake after close"
	_, err = f.core.(*core).inbox.Enqueue(t.Context(), item)
	require.NoError(t, err)
	f.core.(*core).reconciler.Notify()
	resumed := take(t, f.model.opened)
	require.Equal(t, "resume-direct-thread", resumed.config.ResumeToken)
	require.Contains(t, take(t, resumed.inputs).Text, "wake after close")
	require.Equal(t, domain.ActorMember, f.hub.actor("direct-thread"))
	resumed.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	f.messaging.in <- message("direct-thread", "member", "accepted member")
	require.Contains(t, take(t, resumed.inputs).Text, "accepted member")
	select {
	case <-f.model.opened:
		t.Fatal("unexpected session")
	default:
	}
}
