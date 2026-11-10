package discord

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func TestBackfillStoredConversations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		channels []string
		want     map[domain.ConversationRef]bool
	}{
		{"empty", nil, map[domain.ConversationRef]bool{"chat:900:": true}},
		{"stored channels", []string{"400", "400", "901"}, map[domain.ConversationRef]bool{"chat:400:": false, "chat:900:": true, "chat:901:": true}},
		{"stored owner DM", []string{"400", "900", "901"}, map[domain.ConversationRef]bool{"chat:400:": false, "chat:900:": true, "chat:901:": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := make(map[string]int)
			a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case "/channels/901":
					writeJSON(t, w, map[string]any{"id": "901", "type": 1})
				case "/channels/400/messages":
					reads["400"]++
					writeJSON(t, w, []sdk.Message{message(600, 400)})
				case "/channels/900/messages":
					reads["900"]++
					writeJSON(t, w, []sdk.Message{message(700, 900)})
				case "/channels/901/messages":
					reads["901"]++
					writeJSON(t, w, []sdk.Message{message(701, 901)})
				case "/channels/402", "/channels/403", "/channels/404":
					t.Errorf("unexpected backfill target %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				default:
					return false
				}
				return true
			})
			seedConversations(t, a, tc.channels...)
			for _, event := range []domain.InboundEvent{
				{Kind: domain.InboundEventMessage, Conversation: "other:402:"},
				{Kind: domain.InboundEventMessage, Conversation: "chat:403:thread", Parent: "chat:403:"},
				{Kind: domain.InboundEventReaction, Conversation: "chat:404:"},
			} {
				event.Sender = domain.Sender{ID: "200", Kind: domain.SenderHuman}
				event.ReceivedAt = time.Now()
				require.NoError(t, a.history.SaveInbound(t.Context(), event))
			}
			got := make(map[domain.ConversationRef]bool)
			var order []domain.ConversationRef
			require.NoError(t, a.backfill(t.Context(), func(_ context.Context, event domain.InboundEvent) error {
				order = append(order, event.Conversation)
				got[event.Conversation] = event.Direct
				return nil
			}))
			require.Equal(t, tc.want, got)
			require.Equal(t, domain.ConversationRef("chat:900:"), order[0])
			require.Len(t, reads, len(tc.want))
			for channel, count := range reads {
				require.Equal(t, 1, count, channel)
			}
		})
	}
}

type conversationListFailure struct{ domain.EventRepository }

func (conversationListFailure) ListConversations(context.Context, string) ([]domain.Conversation, error) {
	return nil, errors.New("private message fake-token")
}

func TestBackfillListFailureStillIncludesOwner(t *testing.T) {
	logs := captureWarnings(t)
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/channels/900/messages" {
			return false
		}
		writeJSON(t, w, []sdk.Message{message(700, 900)})
		return true
	})
	a.history = conversationListFailure{a.history}
	var got []domain.InboundEvent
	require.NoError(t, a.backfill(t.Context(), func(_ context.Context, event domain.InboundEvent) error {
		got = append(got, event)
		return nil
	}))
	require.Len(t, got, 1)
	require.Equal(t, domain.ConversationRef("chat:900:"), got[0].Conversation)
	require.True(t, got[0].Direct)
	require.Contains(t, logs.String(), `"operation":"ListConversations"`)
	require.NotContains(t, logs.String(), "private message")
	require.NotContains(t, logs.String(), "fake-token")
}
