package discord

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestLookupFailureDropsEventWithoutRestart(t *testing.T) {
	logs := captureWarnings(t)
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/channels/300" {
			return false
		}
		w.WriteHeader(http.StatusForbidden)
		writeJSON(t, w, map[string]any{"message": "private message and fake-token", "code": 50001})
		return true
	})
	opens := 0
	a.createGateway = func(_ string, listener gateway.EventHandlerFunc, _ ...gateway.ConfigOpt) gateway.Gateway {
		opens++
		listener(nil, "", 1, gateway.EventMessageCreate{Message: message(600, 300)})
		listener(nil, "", 2, gateway.EventMessageCreate{Message: message(601, 400)})
		return &stubGateway{}
	}
	stop := errors.New("consume failed")
	consumed := 0
	err := a.Start(t.Context(), func(_ context.Context, e domain.InboundEvent) error {
		consumed++
		assert.Equal(t, "chat:m:601", e.SourceKey)
		return stop
	})
	require.ErrorIs(t, err, stop)
	assert.Equal(t, 1, consumed)
	assert.Equal(t, 1, opens)
	assert.NotContains(t, logs.String(), "private message")
	assert.NotContains(t, logs.String(), "fake-token")
}

func TestThreadParticipationFailureForwardsWithoutCaching(t *testing.T) {
	for _, tt := range []struct{ path, operation string }{
		{"/channels/300/thread-members/100", "GetThreadMember"},
		{"/channels/400/messages/300", "GetMessage"},
	} {
		t.Run(tt.operation, func(t *testing.T) {
			logs := captureWarnings(t)
			var recovered atomic.Bool
			a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == tt.path && !recovered.Load() {
					w.WriteHeader(http.StatusForbidden)
					writeJSON(t, w, map[string]any{"message": "private message and fake-token", "code": 50001})
					return true
				}
				switch r.URL.Path {
				case "/channels/300":
					writeJSON(t, w, map[string]any{"id": "300", "type": 11, "parent_id": "400", "owner_id": "201"})
				case "/channels/300/thread-members/100":
					w.WriteHeader(http.StatusNotFound)
					writeJSON(t, w, map[string]any{"code": 10007})
				case "/channels/400/messages/300":
					w.WriteHeader(http.StatusNotFound)
					writeJSON(t, w, map[string]any{"code": 10008})
				default:
					return false
				}
				return true
			})
			var got []domain.InboundEvent
			stop := errors.New("consume failed")
			consume := func(_ context.Context, event domain.InboundEvent) error {
				got = append(got, event)
				return stop
			}
			require.ErrorIs(t, a.dispatch(t.Context(), gateway.EventMessageCreate{Message: message(600, 300)}, consume), stop)
			require.Len(t, got, 1)
			assert.Equal(t, domain.ConversationRef("chat:400:300"), got[0].Conversation)
			assert.Equal(t, domain.ConversationRef("chat:400:"), got[0].Parent)
			assert.Equal(t, domain.ConversationRef("chat:300:600"), got[0].MessageRef)
			assert.False(t, got[0].MentionsMe)
			assert.False(t, got[0].Direct)
			assert.NotContains(t, logs.String(), "private message")
			assert.NotContains(t, logs.String(), "fake-token")

			recovered.Store(true)
			require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageCreate{Message: message(601, 300)}, consume))
			assert.Len(t, got, 1)
		})
	}
}

func TestBackfillSkipsFailedChannels(t *testing.T) {
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/channels/301/messages", "/users/@me/channels":
			w.WriteHeader(http.StatusForbidden)
			writeJSON(t, w, map[string]any{"code": 50001, "message": "private message and fake-token"})
			return true
		case "/channels/400/messages":
			writeJSON(t, w, []sdk.Message{message(600, 400)})
			return true
		}
		return false
	})
	seedConversations(t, a, "301", "400")
	var got []domain.InboundEvent
	require.NoError(t, a.backfill(t.Context(), func(_ context.Context, e domain.InboundEvent) error {
		got = append(got, e)
		return nil
	}))
	require.Len(t, got, 1)
	assert.Equal(t, "chat:m:600", got[0].SourceKey)
}

func TestBackfillSkipsFailedMessage(t *testing.T) {
	at := time.Now().Add(-time.Hour).UTC()
	earlier, failed, later := message(599, 400), message(600, 300), message(601, 400)
	earlier.CreatedAt, failed.CreatedAt, later.CreatedAt = at, at.Add(time.Minute), at.Add(2*time.Minute)
	edited := at.Add(3 * time.Minute)
	failed.EditedTimestamp, later.EditedTimestamp = &edited, &edited
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/channels/400/messages":
			writeJSON(t, w, []sdk.Message{later, failed, earlier})
			return true
		case "/channels/300":
			w.WriteHeader(http.StatusForbidden)
			writeJSON(t, w, map[string]any{"code": 50001})
			return true
		}
		return false
	})
	seedConversations(t, a, "400")
	var keys []string
	require.NoError(t, a.backfill(t.Context(), func(_ context.Context, event domain.InboundEvent) error {
		keys = append(keys, event.SourceKey)
		return nil
	}))
	assert.Equal(t, []string{"chat:m:599", "chat:m:601", "chat:e:601:" + edited.Format(time.RFC3339Nano)}, keys)
}
