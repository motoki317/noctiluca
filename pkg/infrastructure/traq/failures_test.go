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

func checkWarning(t *testing.T, logs *bytes.Buffer, operation string, status int, ids map[string]string) {
	t.Helper()
	var entry map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
	require.Equal(t, "WARN", entry["level"])
	require.Equal(t, "team", entry["adapter"])
	require.Equal(t, operation, entry["operation"])
	require.EqualValues(t, status, entry["http_status"])
	for key, value := range ids {
		require.Equal(t, value, entry[key], key)
	}
	require.NotContains(t, logs.String(), "private text")
	require.NotContains(t, logs.String(), "test-token")
}

func TestWarnLookupUsesCallerOperation(t *testing.T) {
	logs := captureWarnings(t)
	a := &adapter{name: "team"}
	a.warnLookup(t.Context(), "decode traQ event", errors.New("private text test-token"), "event_type", "MESSAGE_CREATED")
	checkWarning(t, logs, "decode traQ event", 0, map[string]string{"event_type": "MESSAGE_CREATED"})
}

func TestEventLookupFailureSkipsWithoutRestart(t *testing.T) {
	for _, tc := range []struct{ name, path, operation, channel string }{
		{"message", "/api/v3/messages/posted", "read traQ stamp target", ""},
		{"channel", "/api/v3/channels", "resolve traQ stamp channel", "channel"},
		{"stamp", "/api/v3/stamps/stamp", "read traQ stamp", "channel"},
		{"reaction user", "/api/v3/users/human", "read traQ user", "channel"},
		{"delete user", "/api/v3/users/human", "read traQ user", "dm"},
		{"stored author", "", "look up deleted traQ message", "channel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureWarnings(t)
			var connections atomic.Int32
			at := time.Now().Add(time.Minute)
			kind := "BOT_MESSAGE_STAMPS_UPDATED"
			body := payload{MessageID: "posted", Stamps: []sdk.MessageStamp{{StampId: "stamp", UserId: "human", Count: 1, UpdatedAt: at}}}
			if tc.name == "delete user" {
				kind = "DIRECT_MESSAGE_DELETED"
				body = payload{EventTime: at, Message: wireMessage{ID: "posted", Channel: "dm", UserID: "human"}}
			}
			if tc.name == "stored author" {
				kind = "MESSAGE_DELETED"
				body = payload{EventTime: at, Message: wireMessage{ID: "posted", Channel: "channel"}}
			}
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.path {
					w.WriteHeader(403)
					_, _ = w.Write([]byte(`{"message":"private text test-token"}`))
					return
				}
				switch r.URL.Path {
				case "/api/v3/users/me":
					respond(t, w, sdk.MyUserDetail{Id: "bot"})
				case "/api/v3/messages/posted":
					message := restMessage("posted", "channel", "private text", at)
					message.UserId = "bot"
					respond(t, w, message)
				case "/api/v3/channels":
					respond(t, w, sdk.ChannelList{Public: []sdk.Channel{{Id: "channel", Name: "channel"}}})
				case "/api/v3/stamps/stamp":
					respond(t, w, sdk.Stamp{Id: "stamp", Name: "eyes"})
				case "/api/v3/users/human":
					respond(t, w, sdk.UserDetail{Id: "human"})
				case "/api/v3/bots/ws":
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					require.NoError(t, err)
					defer conn.Close()
					connections.Add(1)
					require.NoError(t, conn.WriteJSON(frame(t, kind, body)))
					require.NoError(t, conn.WriteJSON(frame(t, "MESSAGE_CREATED", messagePayload("after", "channel", "other", "private text", at))))
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
			if tc.name == "stored author" {
				a.history.(*historyFake).err = errors.New("private text test-token")
			}
			stop := errors.New("consume failure")
			var events []domain.InboundEvent
			done := make(chan error, 1)
			go func() {
				done <- a.Start(t.Context(), func(_ context.Context, event domain.InboundEvent) error { events = append(events, event); return stop })
			}()
			select {
			case err := <-done:
				require.ErrorIs(t, err, stop)
			case <-time.After(3 * time.Second):
				t.Fatal("consume failure did not stop Start")
			}
			require.EqualValues(t, 1, connections.Load())
			require.Len(t, events, 1)
			require.Equal(t, "team:m:after", events[0].SourceKey)
			ids := map[string]string{"message_id": "posted", "channel_id": tc.channel}
			status := 403
			if tc.name == "stored author" {
				status = 0
			} else {
				ids["user_id"] = "human"
			}
			if kind == "BOT_MESSAGE_STAMPS_UPDATED" {
				ids["stamp_id"] = "stamp"
			}
			checkWarning(t, logs, tc.operation, status, ids)
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

func TestBackfillLookupFailureSkipsWithoutRestart(t *testing.T) {
	for _, tc := range []struct {
		name, path, operation string
		status                int
		ids                   map[string]string
	}{
		{"owner", "/api/v3/users/owner/dm-channel", "resolve traQ direct message", 403, map[string]string{"user_id": "owner"}},
		{"history", "/api/v3/channels/bad/messages", "backfill traQ messages", 403, map[string]string{"channel_id": "bad"}},
		{"user", "/api/v3/users/unknown", "read traQ user", 403, map[string]string{"channel_id": "bad", "message_id": "missed", "user_id": "unknown"}},
		{"cursor", "", "read traQ backfill cursor", 0, map[string]string{"channel_id": "bad"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureWarnings(t)
			var connections atomic.Int32
			at := time.Now().Add(-time.Minute)
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.path {
					w.WriteHeader(403)
					_, _ = w.Write([]byte(`{"message":"private text test-token"}`))
					return
				}
				switch r.URL.Path {
				case "/api/v3/users/me":
					respond(t, w, sdk.MyUserDetail{Id: "bot"})
				case "/api/v3/users/owner/dm-channel":
					respond(t, w, sdk.DMChannel{Id: "dm", UserId: "owner"})
				case "/api/v3/users/human":
					respond(t, w, sdk.UserDetail{Id: "human"})
				case "/api/v3/channels/bad/messages":
					if tc.name == "user" {
						message := restMessage("missed", "bad", "private text", at)
						message.UserId = "unknown"
						respond(t, w, []sdk.Message{restMessage("later", "bad", "private text", at.Add(time.Second)), message, restMessage("earlier", "bad", "private text", at.Add(-time.Second))})
					} else {
						respond(t, w, []sdk.Message{})
					}
				case "/api/v3/channels/dm/messages":
					respond(t, w, []sdk.Message{})
				case "/api/v3/channels/good/messages":
					respond(t, w, []sdk.Message{restMessage("backfilled", "good", "private text", at)})
				case "/api/v3/bots/ws":
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					require.NoError(t, err)
					defer conn.Close()
					connections.Add(1)
					require.NoError(t, conn.WriteJSON(frame(t, "MESSAGE_CREATED", messagePayload("live", "good", "human", "private text", at))))
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
			a.config.Owner = "owner"
			a.config.Channels = []string{"bad", "good"}
			if tc.name == "cursor" {
				a.history = cursorFailure{a.history}
			}
			stop := errors.New("consume failure")
			var events []domain.InboundEvent
			done := make(chan error, 1)
			go func() {
				done <- a.Start(t.Context(), func(_ context.Context, event domain.InboundEvent) error {
					events = append(events, event)
					if event.SourceKey == "team:m:live" {
						return stop
					}
					return nil
				})
			}()
			select {
			case err := <-done:
				require.ErrorIs(t, err, stop)
			case <-time.After(3 * time.Second):
				t.Fatal("consume failure did not stop Start")
			}
			require.EqualValues(t, 1, connections.Load())
			want := []string{"team:m:backfilled", "team:m:live"}
			if tc.name == "user" {
				want = append([]string{"team:m:earlier", "team:m:later"}, want...)
			}
			var keys []string
			for _, event := range events {
				keys = append(keys, event.SourceKey)
			}
			require.Equal(t, want, keys)
			checkWarning(t, logs, tc.operation, tc.status, tc.ids)
		})
	}
}

func TestBackfillConsumeFailureStopsStart(t *testing.T) {
	var connections atomic.Int32
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/users/me":
			respond(t, w, sdk.MyUserDetail{Id: "bot"})
		case "/api/v3/users/human":
			respond(t, w, sdk.UserDetail{Id: "human"})
		case "/api/v3/channels/channel/messages":
			respond(t, w, []sdk.Message{restMessage("backfilled", "channel", "text", time.Now().Add(-time.Minute))})
		case "/api/v3/bots/ws":
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			require.NoError(t, err)
			defer conn.Close()
			connections.Add(1)
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
	a.config.Channels = []string{"channel"}
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
