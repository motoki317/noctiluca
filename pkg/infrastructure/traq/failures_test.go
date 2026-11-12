package traq

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

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	sdk "github.com/traPtitech/go-traq"

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

func TestWarnLookupUsesCallerOperation(t *testing.T) {
	secret := errors.New("private text test-token")
	for _, tc := range []struct {
		name, operation string
		err             error
	}{
		{"plain", "decode traQ event", secret},
		{"API", "read traQ user", apiError("read traQ user", &http.Response{StatusCode: http.StatusForbidden}, secret)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureWarnings(t)
			a := &adapter{name: "team"}
			a.warnLookup(t.Context(), "decode traQ event", tc.err, "event_type", "MESSAGE_CREATED")
			var entry map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
			require.Equal(t, tc.operation, entry["operation"])
			require.Equal(t, "MESSAGE_CREATED", entry["event_type"])
			require.NotContains(t, logs.String(), "private text")
			require.NotContains(t, logs.String(), "test-token")
		})
	}
}

func TestEventLookupFailureSkipsEvent(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"message", "/api/v3/messages/posted"},
		{"stored author", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().Add(time.Minute)
			kind := "BOT_MESSAGE_STAMPS_UPDATED"
			body := payload{MessageID: "posted", Stamps: []sdk.MessageStamp{{StampId: "stamp", UserId: "human", Count: 1, UpdatedAt: at}}}
			if tc.name == "stored author" {
				kind = "MESSAGE_DELETED"
				body = payload{EventTime: at, Message: wireMessage{ID: "posted", Channel: "channel"}}
			}
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, tc.path, r.URL.Path)
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"message":"private text test-token"}`))
			})
			if tc.name == "stored author" {
				a.history.(*historyFake).err = errors.New("private text test-token")
			}
			stop := errors.New("consume failure")
			var events []domain.InboundEvent
			consume := func(_ context.Context, event domain.InboundEvent) error { events = append(events, event); return stop }
			require.NoError(t, a.dispatch(t.Context(), frame(t, kind, body), consume))
			require.Empty(t, events)
			require.ErrorIs(t, a.dispatch(t.Context(), frame(t, "MESSAGE_CREATED", messagePayload("after", "channel", "other", "text", at)), consume), stop)
			require.Len(t, events, 1)
			require.Equal(t, "team:m:after", events[0].SourceKey)
		})
	}
}

type cursorFailure struct{ domain.EventRepository }

func (h cursorFailure) LatestMessageTime(ctx context.Context, ref domain.ConversationRef) (time.Time, error) {
	if ref == Conversation("team", "bad", "") {
		return time.Time{}, errors.New("private text test-token")
	}
	return h.EventRepository.LatestMessageTime(ctx, ref)
}

func TestBackfillLookupFailureSkipsTarget(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"history", "/api/v3/channels/bad/messages"},
		{"user", "/api/v3/users/unknown"},
		{"cursor", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().Add(-time.Minute)
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.path {
					w.WriteHeader(403)
					_, _ = w.Write([]byte(`{"message":"private text test-token"}`))
					return
				}
				switch r.URL.Path {
				case "/api/v3/channels/bad/bots", "/api/v3/channels/good/bots":
					respond(t, w, []sdk.BotUser{{Id: "agent", BotUserId: "bot"}})
				case "/api/v3/users/human":
					respond(t, w, sdk.UserDetail{Id: "human"})
				case "/api/v3/channels/bad/messages":
					message := restMessage("missed", "bad", "private text", at)
					message.UserId = "unknown"
					respond(t, w, []sdk.Message{restMessage("later", "bad", "private text", at.Add(time.Second)), message, restMessage("earlier", "bad", "private text", at.Add(-time.Second))})
				case "/api/v3/channels/good/messages":
					respond(t, w, []sdk.Message{restMessage("backfilled", "good", "private text", at)})
				default:
					t.Errorf("unexpected %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})
			seedConversations(t, a, "bad", "good")
			if tc.name == "cursor" {
				a.history = cursorFailure{a.history}
			}
			var events []domain.InboundEvent
			require.NoError(t, a.backfill(t.Context(), a.backfillTargets(t.Context()), func(_ context.Context, event domain.InboundEvent) error {
				events = append(events, event)
				return nil
			}))
			want := []string{"team:m:backfilled"}
			if tc.name == "user" {
				want = append([]string{"team:m:earlier", "team:m:later"}, want...)
			}
			var keys []string
			for _, event := range events {
				keys = append(keys, event.SourceKey)
			}
			require.Equal(t, want, keys)
		})
	}
}

func TestBackfillConsumeFailureStopsStart(t *testing.T) {
	var connections atomic.Int32
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/users/me":
			respond(t, w, sdk.MyUserDetail{Id: "bot"})
		case "/api/v3/channels/channel/bots":
			respond(t, w, []sdk.BotUser{{Id: "agent", BotUserId: "bot"}})
		case "/api/v3/users/human":
			respond(t, w, sdk.UserDetail{Id: "human"})
		case "/api/v3/channels/channel/messages":
			respond(t, w, []sdk.Message{restMessage("backfilled", "channel", "text", time.Now().Add(-time.Minute))})
		case "/api/v3/bots/ws":
			// Count before the upgrade: once the client reads the 101 response, Start can return before this handler resumes.
			connections.Add(1)
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			require.NoError(t, err)
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	seedConversations(t, a, "channel")
	stop := errors.New("inbox unavailable")
	done := make(chan error, 1)
	go func() { done <- a.Start(t.Context(), func(context.Context, domain.InboundEvent) error { return stop }) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, stop)
	case <-time.After(3 * time.Second):
		t.Fatal("consume failure did not stop Start")
	}
	require.EqualValues(t, 1, connections.Load())
}
