package discord

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func TestLocationCacheSharedByLiveAndBackfill(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	a := testAdapter(t, Config{Channels: []string{"400", "401"}}, func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		calls[r.URL.Path]++
		mu.Unlock()
		for _, channel := range []snowflake.ID{400, 401} {
			if r.URL.Path == "/channels/"+channel.String()+"/messages" {
				writeJSON(t, w, []sdk.Message{message(600, channel), message(601, channel), message(602, channel)})
				return true
			}
		}
		return false
	})
	var got []domain.InboundEvent
	consume := func(_ context.Context, event domain.InboundEvent) error { got = append(got, event); return nil }
	require.NoError(t, a.backfill(t.Context(), consume))
	require.Len(t, got, 6)
	require.NoError(t, a.dispatch(t.Context(), gateway.EventResumed{}, consume))
	require.Len(t, got, 12)
	for _, channel := range []snowflake.ID{400, 401, 900} {
		require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageCreate{Message: message(603, channel)}, consume))
		event := got[len(got)-1]
		assert.Equal(t, Conversation("chat", channel.String(), ""), event.Conversation)
		assert.Equal(t, channel == 900, event.Direct)
		emoji := "test"
		self := snowflake.ID(100)
		require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageReactionAdd{ChannelID: channel, MessageID: 603, MessageAuthorID: &self, UserID: 200, Emoji: sdk.PartialEmoji{Name: &emoji}}, consume))
	}
	mu.Lock()
	defer mu.Unlock()
	for _, channel := range []string{"400", "401", "900"} {
		assert.Equal(t, 1, calls["/channels/"+channel])
	}
}

func TestThreadParticipationCache(t *testing.T) {
	for _, mode := range []string{"owner", "member", "root"} {
		t.Run(mode, func(t *testing.T) {
			var channels, members, roots atomic.Int32
			a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case "/channels/300":
					channels.Add(1)
					owner := "201"
					if mode == "owner" {
						owner = "100"
					}
					writeJSON(t, w, map[string]any{"id": "300", "type": 11, "parent_id": "400", "owner_id": owner})
				case "/channels/300/thread-members/100":
					members.Add(1)
					if mode == "member" {
						writeJSON(t, w, map[string]string{"id": "300", "user_id": "100"})
					} else {
						w.WriteHeader(http.StatusNotFound)
						writeJSON(t, w, map[string]any{"code": 10007})
					}
				case "/channels/400/messages/300":
					roots.Add(1)
					m := message(300, 400)
					m.Author.ID = 100
					writeJSON(t, w, m)
				default:
					return false
				}
				return true
			})
			for _, id := range []snowflake.ID{600, 601, 602} {
				event, err := a.message(t.Context(), message(id, 300), false)
				require.NoError(t, err)
				require.NotNil(t, event)
				assert.Equal(t, domain.ConversationRef("chat:400:300"), event.Conversation)
				assert.Equal(t, domain.ConversationRef("chat:400:"), event.Parent)
				assert.Equal(t, Conversation("chat", "300", id.String()), event.MessageRef)
			}
			assert.True(t, a.joinedThreads[300])
			assert.EqualValues(t, 1, channels.Load())
			assert.EqualValues(t, map[string]int{"owner": 0, "member": 1, "root": 1}[mode], members.Load())
			assert.EqualValues(t, map[string]int{"owner": 0, "member": 0, "root": 1}[mode], roots.Load())
		})
	}
}

func TestUnlistedThreadSkipsParticipationLookup(t *testing.T) {
	var calls atomic.Int32
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		calls.Add(1)
		switch r.URL.Path {
		case "/channels/300":
			writeJSON(t, w, map[string]any{"id": "300", "type": 11, "parent_id": "400", "owner_id": "201"})
		case "/channels/300/thread-members/100":
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"code": 10007})
		case "/channels/400/messages/300":
			m := message(300, 400)
			m.Author.ID = 100
			writeJSON(t, w, m)
		default:
			return false
		}
		return true
	})
	_, err := a.location(t.Context(), 300)
	require.NoError(t, err)
	calls.Store(0)
	filtered := false
	a.SetConversationFilter(func(ref domain.ConversationRef, direct bool) bool {
		filtered = true
		assert.Equal(t, domain.ConversationRef("chat:400:"), ref)
		assert.False(t, direct)
		return false
	})
	event, err := a.message(t.Context(), message(600, 300), false)
	require.NoError(t, err)
	require.NotNil(t, event)
	assert.Equal(t, domain.ConversationRef("chat:400:"), event.Parent)
	assert.True(t, filtered)
	assert.Zero(t, calls.Load())
	assert.Empty(t, a.joinedThreads)
}

func TestThreadParticipationRetriesNegativeAndFailedLookups(t *testing.T) {
	for _, mode := range []string{"not joined", "member error", "root error"} {
		t.Run(mode, func(t *testing.T) {
			var members atomic.Int32
			a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case "/channels/300":
					writeJSON(t, w, map[string]any{"id": "300", "type": 11, "parent_id": "400", "owner_id": "201"})
				case "/channels/300/thread-members/100":
					if members.Add(1) > 1 {
						writeJSON(t, w, map[string]string{"id": "300", "user_id": "100"})
					} else if mode == "member error" {
						w.WriteHeader(http.StatusForbidden)
						writeJSON(t, w, map[string]any{"code": 50001})
					} else {
						w.WriteHeader(http.StatusNotFound)
						writeJSON(t, w, map[string]any{"code": 10007})
					}
				case "/channels/400/messages/300":
					if mode == "root error" {
						w.WriteHeader(http.StatusForbidden)
						writeJSON(t, w, map[string]any{"code": 50001})
					} else {
						writeJSON(t, w, message(300, 400))
					}
				default:
					return false
				}
				return true
			})
			event, err := a.message(t.Context(), message(600, 300), false)
			require.NoError(t, err)
			if mode == "not joined" {
				assert.Nil(t, event)
			} else {
				assert.NotNil(t, event)
			}
			for _, id := range []snowflake.ID{601, 602} {
				event, err = a.message(t.Context(), message(id, 300), false)
				require.NoError(t, err)
				require.NotNil(t, event)
			}
			assert.EqualValues(t, 2, members.Load())
		})
	}
}

func TestLocationCacheRetriesFailedLookup(t *testing.T) {
	var calls atomic.Int32
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/channels/400" {
			return false
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			writeJSON(t, w, map[string]any{"code": 50001})
			return true
		}
		return false
	})
	event, err := a.message(t.Context(), message(600, 400), false)
	require.Error(t, err)
	assert.Nil(t, event)
	for _, id := range []snowflake.ID{601, 602} {
		event, err = a.message(t.Context(), message(id, 400), false)
		require.NoError(t, err)
		require.NotNil(t, event)
	}
	assert.EqualValues(t, 2, calls.Load())
}
