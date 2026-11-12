package traq

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdk "github.com/traPtitech/go-traq"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func TestBackfillStoredConversations(t *testing.T) {
	reads := make(map[string]int)
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/users/owner/dm-channel":
			respond(t, w, sdk.DMChannel{Id: "owner_dm", UserId: "owner"})
		case "/api/v3/channels/room/bots":
			respond(t, w, []sdk.BotUser{{Id: "agent", BotUserId: "bot"}})
		case "/api/v3/channels/room/messages", "/api/v3/channels/owner_dm/messages", "/api/v3/channels/peer_dm/messages":
			channel := "room"
			switch r.URL.Path {
			case "/api/v3/channels/owner_dm/messages":
				channel = "owner_dm"
			case "/api/v3/channels/peer_dm/messages":
				channel = "peer_dm"
			}
			reads[channel]++
			respond(t, w, []sdk.Message{restMessage("message-"+channel, channel, "text", time.Now().Add(-time.Minute))})
		case "/api/v3/users/human":
			respond(t, w, sdk.UserDetail{Id: "human"})
		default:
			t.Errorf("unexpected backfill request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "events.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a.history = repository.NewEventRepository(db)
	a.config.Owner = "owner"
	accepted := true
	for _, channel := range []string{"room", "owner_dm", "peer_dm"} {
		ref := Conversation(a.name, channel, "")
		require.NoError(t, a.history.SaveInbound(t.Context(), domain.InboundEvent{
			Kind: domain.InboundEventMessage, Conversation: ref, Direct: channel != "room", Accepted: &accepted,
			Sender: sender("human", "Human", false), Text: "stored message", ReceivedAt: time.Now().Add(-25 * time.Hour),
		}))
	}
	got := make(map[domain.ConversationRef]bool)
	var order []domain.ConversationRef
	targets := a.backfillTargets(t.Context())
	require.Len(t, targets, 3)
	require.NoError(t, a.backfill(t.Context(), targets, func(_ context.Context, event domain.InboundEvent) error {
		order = append(order, event.Conversation)
		got[event.Conversation] = event.Direct
		return nil
	}))
	require.Equal(t, map[domain.ConversationRef]bool{"team:room:": false, "team:owner_dm:": true, "team:peer_dm:": true}, got)
	require.Equal(t, domain.ConversationRef("team:owner_dm:"), order[0])
	require.Equal(t, map[string]int{"room": 1, "owner_dm": 1, "peer_dm": 1}, reads)
}

type conversationListFailure struct{ domain.EventRepository }

func (conversationListFailure) ListConversations(context.Context, string) ([]domain.Conversation, error) {
	return nil, errors.New("private text test-token")
}

func TestBackfillListFailureStillIncludesOwner(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/users/owner/dm-channel":
			respond(t, w, sdk.DMChannel{Id: "dm", UserId: "owner"})
		case "/api/v3/channels/dm/messages":
			respond(t, w, []sdk.Message{restMessage("message", "dm", "text", time.Now().Add(-time.Minute))})
		case "/api/v3/users/human":
			respond(t, w, sdk.UserDetail{Id: "human"})
		default:
			t.Errorf("unexpected backfill request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	a.config.Owner = "owner"
	a.history = conversationListFailure{a.history}
	targets := a.backfillTargets(t.Context())
	require.Len(t, targets, 1)
	require.Equal(t, "dm", targets[0].channel)
	require.True(t, targets[0].direct)
	var got []domain.InboundEvent
	require.NoError(t, a.backfill(t.Context(), targets, func(_ context.Context, event domain.InboundEvent) error {
		got = append(got, event)
		return nil
	}))
	require.Len(t, got, 1)
	require.True(t, got[0].Direct)
}

func TestBackfillMatchesLiveChannelDelivery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		joined bool
		direct bool
		status int
		want   []string
	}{
		{"joined", true, false, http.StatusOK, []string{"plain", "mention", "other", "malformed"}},
		{"not joined", false, false, http.StatusOK, []string{"mention"}},
		{"lookup failed", false, false, http.StatusForbidden, []string{"mention"}},
		{"DM", false, true, http.StatusForbidden, []string{"plain", "mention", "other", "malformed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().Add(-time.Minute)
			var membershipLookups int
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v3/channels/room/bots":
					membershipLookups++
					w.WriteHeader(tc.status)
					if tc.status != http.StatusOK {
						respond(t, w, map[string]string{"message": "private text test-token"})
						return
					}
					bots := []sdk.BotUser{{Id: "bot", BotUserId: "other"}}
					if tc.joined {
						bots = append(bots, sdk.BotUser{Id: "agent", BotUserId: "bot"})
					}
					respond(t, w, bots)
				case "/api/v3/channels/room/messages":
					respond(t, w, []sdk.Message{
						restMessage("malformed", "room", `!{"type":"user"`, at.Add(3*time.Second)),
						restMessage("other", "room", `!{"type":"user","id":"other"}`, at.Add(2*time.Second)),
						restMessage("mention", "room", `!{"type":"user","id":"bot"}`, at.Add(time.Second)),
						restMessage("plain", "room", "text", at),
					})
				case "/api/v3/users/human":
					respond(t, w, sdk.UserDetail{Id: "human"})
				default:
					t.Errorf("unexpected backfill request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			target := backfillTarget{channel: "room", direct: tc.direct, since: at.Add(-time.Hour), until: time.Now()}
			var ids []string
			require.NoError(t, a.backfill(t.Context(), []backfillTarget{target}, func(_ context.Context, event domain.InboundEvent) error {
				_, _, id, ok := domain.ParseConversationRef(event.MessageRef)
				require.True(t, ok)
				require.Equal(t, tc.direct, event.Direct)
				require.Equal(t, id == "mention", event.MentionsMe)
				ids = append(ids, id)
				return nil
			}))
			require.Equal(t, tc.want, ids)
			if tc.direct {
				require.Zero(t, membershipLookups)
			} else {
				require.Equal(t, 1, membershipLookups)
			}
		})
	}
}
