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
	f := setup(t, Config{}, []Messaging{{Owner: "owner", GroupRefreshInterval: 10 * time.Millisecond, Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Groups: []string{"group"}}}}}, fixtureOptions{groups: provider})
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
	f := setup(t, Config{}, []Messaging{{Owner: "owner", UserGroups: map[string][]string{"group": {"owner", "member"}}, Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Groups: []string{"group"}}}}}, fixtureOptions{})
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

func TestEntryTrustRefreshAndRestore(t *testing.T) {
	f := setup(t, Config{}, []Messaging{{Owner: "owner", Trust: domain.AcceptRule{Groups: []string{"team"}}, UserGroups: map[string][]string{"team": {"owner"}}}}, fixtureOptions{})
	e := message("test:direct-thread", "owner", "first")
	e.Parent = "test:direct"
	e.Direct = true
	f.messaging.in <- e
	s := take(t, f.model.opened)
	require.Contains(t, take(t, s.inputs).Text, "first")
	require.NoError(t, f.core.(*core).applyGroups(f.core.(*core).messaging.instances[0], map[string][]string{"team": {"owner", "member"}}))
	e.Sender.ID = "member"
	e.Text = "accepted without restart"
	f.messaging.in <- e
	require.Contains(t, take(t, s.inputs).Text, "accepted without restart")
	require.NoError(t, s.Close())
	require.Equal(t, domain.SessionKey("test:direct"), take(t, f.hub.closed))
	item, err := f.core.(*core).workItem(t.Context(), "test:direct")
	require.NoError(t, err)
	require.Equal(t, domain.SessionKey("test:direct"), item.SessionKey)
	require.Equal(t, domain.ConversationRef("test:direct"), item.Payload.Context.Conversation)
	require.True(t, item.Payload.Context.Direct)
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
}
