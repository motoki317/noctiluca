package discord

import (
	"bytes"
	"context"
	"encoding/json"
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
	at := time.Now()
	emoji := "test"
	for _, tt := range []struct {
		name, path, operation string
		event                 gateway.EventData
	}{
		{"channel", "/channels/300", "GetChannel", gateway.EventMessageCreate{Message: message(600, 300)}},
		{"update", "/channels/300/messages/600", "GetMessage", gateway.EventMessageUpdate{Message: sdk.Message{ID: 600, ChannelID: 300, EditedTimestamp: &at}}},
		{"reaction message", "/channels/300/messages/600", "GetMessage", gateway.EventMessageReactionAdd{UserID: 200, ChannelID: 300, MessageID: 600, Emoji: sdk.PartialEmoji{Name: &emoji}}},
		{"reaction user", "/users/200", "GetUser", gateway.EventMessageReactionAdd{UserID: 200, ChannelID: 300, MessageID: 600, Emoji: sdk.PartialEmoji{Name: &emoji}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureWarnings(t)
			a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == tt.path {
					w.WriteHeader(http.StatusForbidden)
					writeJSON(t, w, map[string]any{"message": "private message and fake-token", "code": 50001})
					return true
				}
				switch r.URL.Path {
				case "/channels/300":
					writeJSON(t, w, map[string]any{"id": "300", "type": 11, "parent_id": "400", "owner_id": "201"})
					return true
				case "/channels/300/messages/600":
					m := message(600, 300)
					m.Author.ID = 100
					writeJSON(t, w, m)
					return true
				}
				return false
			})
			opens := 0
			a.createGateway = func(_ string, listener gateway.EventHandlerFunc, _ ...gateway.ConfigOpt) gateway.Gateway {
				opens++
				listener(nil, "", 1, tt.event)
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
			var entry map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
			assert.Equal(t, "WARN", entry["level"])
			assert.Equal(t, tt.operation, entry["operation"])
			assert.Equal(t, "300", entry["channel_id"])
			assert.Equal(t, "600", entry["message_id"])
			assert.Equal(t, float64(403), entry["http_status"])
			assert.NotContains(t, logs.String(), "private message")
			assert.NotContains(t, logs.String(), "fake-token")
		})
	}
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
			var entry map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
			assert.Equal(t, "WARN", entry["level"])
			assert.Equal(t, "Discord lookup failed", entry["msg"])
			assert.Equal(t, tt.operation, entry["operation"])
			assert.Equal(t, "300", entry["channel_id"])
			assert.Equal(t, "600", entry["message_id"])
			assert.Equal(t, float64(403), entry["http_status"])
			assert.NotContains(t, logs.String(), "private message")
			assert.NotContains(t, logs.String(), "fake-token")

			recovered.Store(true)
			logs.Reset()
			require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageCreate{Message: message(601, 300)}, consume))
			assert.Len(t, got, 1)
			assert.Empty(t, logs.String())
		})
	}
}

func TestBackfillSkipsFailedChannels(t *testing.T) {
	logs := captureWarnings(t)
	a := testAdapter(t, Config{Channels: []string{"300", "301", "400"}}, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/channels/300", "/channels/301/messages", "/users/@me/channels":
			w.WriteHeader(http.StatusForbidden)
			writeJSON(t, w, map[string]any{"code": 50001, "message": "private message and fake-token"})
			return true
		case "/channels/400/messages":
			writeJSON(t, w, []sdk.Message{message(600, 400)})
			return true
		}
		return false
	})
	var got []domain.InboundEvent
	require.NoError(t, a.backfill(t.Context(), func(_ context.Context, e domain.InboundEvent) error {
		got = append(got, e)
		return nil
	}))
	require.Len(t, got, 1)
	assert.Equal(t, "chat:m:600", got[0].SourceKey)
	decoder := json.NewDecoder(logs)
	for _, expected := range []struct{ op, channel string }{{"CreateDMChannel", "0"}, {"GetChannel", "300"}, {"GetMessages", "301"}} {
		var entry map[string]any
		require.NoError(t, decoder.Decode(&entry))
		assert.Equal(t, "WARN", entry["level"])
		assert.Equal(t, expected.op, entry["operation"])
		assert.Equal(t, expected.channel, entry["channel_id"])
		assert.Equal(t, "0", entry["message_id"])
		assert.Equal(t, float64(403), entry["http_status"])
	}
	stop := errors.New("inbox unavailable")
	require.ErrorIs(t, a.backfill(t.Context(), func(context.Context, domain.InboundEvent) error { return stop }), stop)
}

func TestBackfillMessageFailureIncludesMessageID(t *testing.T) {
	logs := captureWarnings(t)
	a := testAdapter(t, Config{Channels: []string{"400"}}, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/channels/400/messages":
			// A malformed page must still identify the message that failed conversion.
			writeJSON(t, w, []sdk.Message{message(600, 300)})
			return true
		case "/channels/300":
			w.WriteHeader(http.StatusForbidden)
			writeJSON(t, w, map[string]any{"code": 50001})
			return true
		}
		return false
	})
	require.NoError(t, a.backfill(t.Context(), func(context.Context, domain.InboundEvent) error {
		t.Fatal("consume after failed message lookup")
		return nil
	}))
	var entry map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
	assert.Equal(t, "GetChannel", entry["operation"])
	assert.Equal(t, "300", entry["channel_id"])
	assert.Equal(t, "600", entry["message_id"])
	assert.Equal(t, float64(403), entry["http_status"])
}
