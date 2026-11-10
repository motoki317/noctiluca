package traq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	sdk "github.com/traPtitech/go-traq"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func messagePayload(id, channel, user, text string, at time.Time) payload {
	m := wireMessage{ID: id, Channel: channel, Text: text, CreatedAt: at, UpdatedAt: at}
	m.User.ID = user
	m.User.DisplayName = "Sender"
	return payload{EventTime: at, Message: m}
}
func frame(t *testing.T, kind string, body payload) envelope {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return envelope{Type: kind, Body: raw}
}

func TestEventMapping(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected API %s", r.URL.Path) })
	a.self = "bot"
	at := time.Now().UTC()
	body := messagePayload("message", "channel", "human", `!{"type":"user","id":"bot"} hi`, at)
	var events []domain.InboundEvent
	consume := func(_ context.Context, event domain.InboundEvent) error { events = append(events, event); return nil }
	for _, kind := range []string{"MESSAGE_CREATED", "MENTION_MESSAGE_CREATED", "MESSAGE_UPDATED", "DIRECT_MESSAGE_CREATED", "DIRECT_MESSAGE_UPDATED"} {
		require.NoError(t, a.dispatch(t.Context(), frame(t, kind, body), consume))
	}
	require.Len(t, events, 5)
	for _, event := range events {
		require.Equal(t, Conversation("team", "channel", ""), event.Conversation)
		require.Empty(t, event.Parent)
		require.Equal(t, Conversation("team", "channel", "message"), event.MessageRef)
		require.Equal(t, domain.SenderHuman, event.Sender.Kind)
		require.Equal(t, "Sender", event.Sender.DisplayName)
		require.True(t, event.MentionsMe)
	}
	require.Equal(t, "team:m:message", events[0].SourceKey)
	require.Equal(t, events[0].SourceKey, events[1].SourceKey)
	require.Equal(t, "team:e:message:"+at.Format(time.RFC3339Nano), events[2].SourceKey)
	require.Equal(t, domain.InboundEventMessageEdited, events[2].Kind)
	require.True(t, events[3].Direct)
	require.False(t, events[0].Direct)
	body.Message.User.ID = "bot"
	require.NoError(t, a.dispatch(t.Context(), frame(t, "MESSAGE_CREATED", body), consume))
	body.Message.User.ID = "other-bot"
	body.Message.User.Bot = true
	require.NoError(t, a.dispatch(t.Context(), frame(t, "MESSAGE_CREATED", body), consume))
	require.Len(t, events, 6)
	require.Equal(t, domain.SenderBot, events[5].Sender.Kind)
}

func TestDeletesRecoverAuthor(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected API %s", r.URL.Path) })
	a.self = "bot"
	at := time.Now().UTC()
	known := domain.InboundEvent{MessageRef: Conversation("team", "channel", "known"), Sender: sender("human", "Sender", false)}
	a.history.(*historyFake).inbound = []domain.InboundEvent{known}
	var events []domain.InboundEvent
	consume := func(_ context.Context, event domain.InboundEvent) error { events = append(events, event); return nil }
	for _, kind := range []string{"MESSAGE_DELETED", "DIRECT_MESSAGE_DELETED"} {
		for _, id := range []string{"known", "unknown"} {
			body := payload{EventTime: at, Message: wireMessage{ID: id, Channel: "channel"}}
			require.NoError(t, a.dispatch(t.Context(), frame(t, kind, body), consume))
		}
	}
	require.Len(t, events, 2)
	for _, event := range events {
		require.Equal(t, known.Sender, event.Sender)
		require.True(t, event.Deleted)
		require.Equal(t, "team:d:known", event.SourceKey)
		require.Empty(t, event.Text)
		require.Equal(t, domain.InboundEventMessageEdited, event.Kind)
		require.False(t, event.MentionsMe)
		require.Empty(t, event.Attachments)
	}
	a.history.(*historyFake).err = errors.New("repository unavailable")
	err := a.dispatch(t.Context(), frame(t, "MESSAGE_DELETED", payload{EventTime: at, Message: wireMessage{ID: "known", Channel: "channel"}}), consume)
	require.NoError(t, err)
	require.Len(t, events, 2)
}

func TestStampSnapshots(t *testing.T) {
	var userReads, stampReads, messageReads atomic.Int32
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/messages/posted":
			messageReads.Add(1)
			message := restMessage("posted", "channel", "reply", time.Now())
			message.UserId = "bot"
			respond(t, w, message)
		case "/api/v3/channels":
			respond(t, w, sdk.ChannelList{Public: []sdk.Channel{{Id: "channel", Name: "channel"}}})
		case "/api/v3/users/human":
			userReads.Add(1)
			respond(t, w, sdk.UserDetail{Id: "human", DisplayName: "Human"})
		case "/api/v3/stamps/stamp":
			stampReads.Add(1)
			respond(t, w, sdk.Stamp{Id: "stamp", Name: "no_good"})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	a.self = "bot"
	a.started = time.Now().Add(-time.Hour)
	at := a.started.Add(time.Minute)
	makeStamp := func(user string, at time.Time) sdk.MessageStamp {
		return sdk.MessageStamp{StampId: "stamp", UserId: user, Count: 1, CreatedAt: at, UpdatedAt: at}
	}
	var events []domain.InboundEvent
	consume := func(_ context.Context, event domain.InboundEvent) error { events = append(events, event); return nil }
	for _, stamps := range [][]sdk.MessageStamp{
		{makeStamp("human", a.started.Add(-time.Second)), makeStamp("bot", at)},
		{makeStamp("human", at)},
		{makeStamp("human", at)},
		{},
		{makeStamp("human", at.Add(-time.Second))},
		{makeStamp("human", at.Add(time.Second))},
	} {
		require.NoError(t, a.reactions(t.Context(), payload{MessageID: "posted", Stamps: stamps}, consume))
	}
	require.Len(t, events, 2)
	require.Equal(t, "team:r:posted:human:stamp", events[0].SourceKey)
	require.Equal(t, events[0].SourceKey, events[1].SourceKey)
	require.Equal(t, ":no_good: on message posted", events[0].Text)
	require.Equal(t, domain.SenderHuman, events[0].Sender.Kind)
	require.Empty(t, events[0].Parent)
	require.Equal(t, Conversation("team", "channel", ""), events[0].Conversation)
	require.EqualValues(t, 1, userReads.Load())
	require.EqualValues(t, 1, stampReads.Load())
	require.EqualValues(t, 1, messageReads.Load())
}

func TestDeniedStampsSkipSenderAndStampLookup(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected API %s", r.URL.Path) })
	a.started = time.Now().Add(-time.Hour)
	a.self = "bot"
	a.posted.Store("posted", "channel")
	a.direct.Store("channel", "")
	a.SetConversationFilter(func(ref domain.ConversationRef, direct bool) bool {
		require.False(t, direct)
		return ref == Conversation("team", "accepted", "")
	})
	require.NoError(t, a.reactions(t.Context(), payload{MessageID: "posted", Stamps: []sdk.MessageStamp{{StampId: "stamp", UserId: "user", Count: 1, UpdatedAt: time.Now()}}}, func(context.Context, domain.InboundEvent) error { t.Error("denied event consumed"); return nil }))
}

func TestStampOnDirectMessage(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/messages/posted":
			message := restMessage("posted", "dm", "reply", time.Now())
			message.UserId = "bot"
			respond(t, w, message)
		case "/api/v3/channels":
			respond(t, w, sdk.ChannelList{Public: []sdk.Channel{}, Dm: []sdk.DMChannel{{Id: "dm", UserId: "human"}}})
		case "/api/v3/users/human":
			respond(t, w, sdk.UserDetail{Id: "human", Bot: true})
		case "/api/v3/stamps/stamp":
			respond(t, w, sdk.Stamp{Id: "stamp", Name: "eyes"})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	a.self = "bot"
	a.started = time.Now().Add(-time.Hour)
	require.NoError(t, a.reactions(t.Context(), payload{MessageID: "posted", Stamps: []sdk.MessageStamp{{StampId: "stamp", UserId: "human", Count: 1, UpdatedAt: time.Now()}}}, func(_ context.Context, event domain.InboundEvent) error {
		require.True(t, event.Direct)
		require.Equal(t, domain.SenderBot, event.Sender.Kind)
		return nil
	}))
}

func TestBackfillCursorAndCap(t *testing.T) {
	now := time.Now().UTC()
	var historyCalls atomic.Int32
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/users/owner/dm-channel":
			respond(t, w, sdk.DMChannel{Id: "dm", UserId: "owner"})
		case "/api/v3/channels/channel/bots":
			respond(t, w, []sdk.BotUser{{Id: "agent", BotUserId: "bot"}})
		case "/api/v3/channels/channel/messages", "/api/v3/channels/dm/messages":
			historyCalls.Add(1)
			since, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("since"))
			require.NoError(t, err)
			require.Equal(t, "200", r.URL.Query().Get("limit"))
			require.Equal(t, "desc", r.URL.Query().Get("order"))
			require.Equal(t, "true", r.URL.Query().Get("inclusive"))
			channel := "channel"
			if r.URL.Path == "/api/v3/channels/dm/messages" {
				channel = "dm"
				require.WithinDuration(t, now.Add(-24*time.Hour), since, time.Second)
			} else {
				require.WithinDuration(t, now.Add(-time.Hour), since, time.Second)
			}
			respond(t, w, []sdk.Message{restMessage("new", channel, `!{"type":"user","id":"bot"}`, now.Add(-time.Minute)), restMessage("old", channel, "old", since), restMessage("too-old", channel, "old", since.Add(-time.Second))})
		case "/api/v3/users/human":
			respond(t, w, sdk.UserDetail{Id: "human"})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	a.self = "bot"
	a.config.Owner = "owner"
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "events.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a.history = repository.NewEventRepository(db)
	a.direct.Store("channel", "")
	for _, event := range []domain.InboundEvent{
		{Kind: domain.InboundEventMessage, ReceivedAt: now.Add(-2 * time.Hour)},
		{Kind: domain.InboundEventMessageEdited, ReceivedAt: now.Add(-time.Hour)},
		{Kind: domain.InboundEventReaction, ReceivedAt: now.Add(-2 * time.Minute)},
		{Kind: domain.InboundEventMessageEdited, Deleted: true, ReceivedAt: now.Add(-time.Minute)},
	} {
		event.Conversation = Conversation("team", "channel", "")
		accepted := true
		event.Accepted = &accepted
		event.Sender = sender("human", "Human", false)
		require.NoError(t, a.history.SaveInbound(t.Context(), event))
	}
	targets := a.backfillTargets(t.Context())
	require.Len(t, targets, 2)
	var events []domain.InboundEvent
	require.NoError(t, a.backfill(t.Context(), targets, func(_ context.Context, event domain.InboundEvent) error { events = append(events, event); return nil }))
	require.Len(t, events, 4)
	require.True(t, events[0].Direct)
	require.Equal(t, "team:m:old", events[2].SourceKey)
	require.Equal(t, "team:m:new", events[3].SourceKey)
	require.True(t, events[3].MentionsMe)
	require.EqualValues(t, 2, historyCalls.Load())
}

func TestWebSocketReconnectBackfillAndShutdown(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	var connections, backfills, delivered atomic.Int32
	firstDelivered := make(chan struct{})
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/users/me":
			respond(t, w, sdk.MyUserDetail{Id: "bot"})
		case "/api/v3/channels/channel/bots":
			respond(t, w, []sdk.BotUser{{Id: "agent", BotUserId: "bot"}})
		case "/api/v3/channels/channel/messages":
			backfills.Add(1)
			respond(t, w, []sdk.Message{restMessage("message", "channel", "hi", at)})
		case "/api/v3/users/human":
			respond(t, w, sdk.UserDetail{Id: "human"})
		case "/api/v3/bots/ws":
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			require.NoError(t, err)
			defer conn.Close()
			connection := connections.Add(1)
			require.NoError(t, conn.WriteJSON(frame(t, "MESSAGE_CREATED", messagePayload("message", "channel", "human", "hi", at))))
			if connection == 1 {
				<-firstDelivered
				return
			}
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
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "events.db")})
	require.NoError(t, err)
	defer db.Close()
	a.history = repository.NewEventRepository(db)
	seedConversations(t, a, "channel")
	inbox := repository.NewInboxRepository(db)
	events := make(chan domain.InboundEvent, 10)
	done := make(chan error, 1)
	go func() {
		done <- a.Start(t.Context(), func(ctx context.Context, event domain.InboundEvent) error {
			err := inbox.Receive(ctx, event, domain.InboxItem{SourceKey: event.SourceKey, Lane: "conv", Payload: domain.InboxPayload{Event: &event}})
			if err == nil {
				events <- event
				if delivered.Add(1) == 2 {
					close(firstDelivered)
				}
			}
			return err
		})
	}()
	for range 4 {
		select {
		case <-events:
		case <-time.After(5 * time.Second):
			t.Fatal("missing live/backfill event")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, a.Shutdown(ctx))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("shutdown did not join")
	}
	require.EqualValues(t, 2, connections.Load())
	require.EqualValues(t, 2, backfills.Load())
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM events").Scan(&count))
	require.Equal(t, 2, count)
}

func TestBackfillKeepsLatestTwoHundred(t *testing.T) {
	now := time.Now().UTC()
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/channels/channel/bots" {
			respond(t, w, []sdk.BotUser{{Id: "agent", BotUserId: "bot"}})
			return
		}
		if r.URL.Path == "/api/v3/users/human" {
			respond(t, w, sdk.UserDetail{Id: "human"})
			return
		}
		require.Equal(t, "200", r.URL.Query().Get("limit"))
		messages := make([]sdk.Message, 205)
		for i := range messages {
			messages[i] = restMessage(fmt.Sprintf("m%d", i), "channel", "text", now.Add(-time.Duration(i)*time.Second))
		}
		respond(t, w, messages)
	})
	a.self = "bot"
	var events []domain.InboundEvent
	require.NoError(t, a.backfill(t.Context(), []backfillTarget{{channel: "channel", since: now.Add(-time.Hour), until: now}}, func(_ context.Context, event domain.InboundEvent) error { events = append(events, event); return nil }))
	require.Len(t, events, 200)
	require.Equal(t, "team:m:m199", events[0].SourceKey)
	require.Equal(t, "team:m:m0", events[199].SourceKey)
}

func TestShutdownInterruptsReconnectWait(t *testing.T) {
	attempted := make(chan struct{})
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/users/me" {
			respond(t, w, sdk.MyUserDetail{Id: "bot"})
			return
		}
		w.WriteHeader(503)
		close(attempted)
	})
	done := make(chan error, 1)
	go func() { done <- a.Start(t.Context(), func(context.Context, domain.InboundEvent) error { return nil }) }()
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("no connection attempt")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, a.Shutdown(ctx))
	require.NoError(t, <-done)
}

func TestDirectDeleteWithAuthorNeedsNoStoredMessage(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v3/users/human", r.URL.Path)
		respond(t, w, sdk.UserDetail{Id: "human", DisplayName: "Human"})
	})
	a.self = "bot"
	a.history.(*historyFake).err = errors.New("history must not be read")
	body := payload{EventTime: time.Now(), Message: wireMessage{ID: "unknown", Channel: "dm", UserID: "human"}}
	var got domain.InboundEvent
	require.NoError(t, a.dispatch(t.Context(), frame(t, "DIRECT_MESSAGE_DELETED", body), func(_ context.Context, event domain.InboundEvent) error { got = event; return nil }))
	require.True(t, got.Deleted)
	require.True(t, got.Direct)
	require.Equal(t, "human", got.Sender.ID)
	require.Equal(t, domain.SenderHuman, got.Sender.Kind)
}

func TestReconnectDoesNotSkipInterruptedBackfill(t *testing.T) {
	at := time.Now().Add(-time.Hour).UTC()
	historyEntered := make(chan struct{})
	var calls, connections atomic.Int32
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/users/me":
			respond(t, w, sdk.MyUserDetail{Id: "bot"})
		case "/api/v3/channels/channel/bots":
			respond(t, w, []sdk.BotUser{{Id: "agent", BotUserId: "bot"}})
		case "/api/v3/users/human":
			respond(t, w, sdk.UserDetail{Id: "human"})
		case "/api/v3/channels/channel/messages":
			since, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("since"))
			require.NoError(t, err)
			require.False(t, since.After(at))
			if calls.Add(1) == 1 {
				close(historyEntered)
				<-r.Context().Done()
				return
			}
			respond(t, w, []sdk.Message{restMessage("missed", "channel", "missed", at)})
		case "/api/v3/bots/ws":
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			require.NoError(t, err)
			defer conn.Close()
			connection := connections.Add(1)
			require.NoError(t, conn.WriteJSON(frame(t, "MESSAGE_CREATED", messagePayload("live", "channel", "human", "live", at.Add(time.Minute)))))
			<-historyEntered
			if connection == 1 {
				return
			}
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
	history := a.history.(*historyFake)
	events := make(chan domain.InboundEvent, 4)
	done := make(chan error, 1)
	go func() {
		done <- a.Start(t.Context(), func(_ context.Context, event domain.InboundEvent) error {
			history.mu.Lock()
			history.inbound = append([]domain.InboundEvent{event}, history.inbound...)
			history.mu.Unlock()
			events <- event
			return nil
		})
	}()
	for _, want := range []string{"team:m:missed", "team:m:live"} {
		select {
		case event := <-events:
			require.Equal(t, want, event.SourceKey)
		case <-time.After(5 * time.Second):
			t.Fatal("missing replay")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, a.Shutdown(ctx))
	require.NoError(t, <-done)
	require.EqualValues(t, 2, calls.Load())
}

func TestDisconnectDrainsBufferedEditsDeletesAndStamps(t *testing.T) {
	stampRead := make(chan struct{})
	releaseStamp := make(chan struct{})
	framesSent := make(chan struct{})
	replayContext := make(chan context.Context, 1)
	at := time.Now().UTC()
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/channels/channel/bots":
			respond(t, w, []sdk.BotUser{{Id: "agent", BotUserId: "bot"}})
		case "/api/v3/channels/channel/messages":
			respond(t, w, []sdk.Message{restMessage("backfilled", "channel", "old", at.Add(-time.Minute))})
		case "/api/v3/stamps/stamp":
			close(stampRead)
			<-releaseStamp
			respond(t, w, sdk.Stamp{Id: "stamp", Name: "eyes"})
		case "/api/v3/users/human":
			respond(t, w, sdk.UserDetail{Id: "human"})
		case "/api/v3/bots/ws":
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.WriteJSON(frame(t, "BOT_MESSAGE_STAMPS_UPDATED", payload{MessageID: "posted", Stamps: []sdk.MessageStamp{{StampId: "stamp", UserId: "human", Count: 1, UpdatedAt: at}}})))
			<-stampRead
			require.NoError(t, conn.WriteJSON(frame(t, "MESSAGE_UPDATED", messagePayload("edited", "channel", "human", "changed", at))))
			require.NoError(t, conn.WriteJSON(frame(t, "DIRECT_MESSAGE_DELETED", payload{EventTime: at, Message: wireMessage{ID: "deleted", Channel: "dm", UserID: "human"}})))
			require.NoError(t, conn.Close())
			close(framesSent)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	seedConversations(t, a, "channel")
	a.self = "bot"
	a.started = at.Add(-time.Hour)
	a.posted.Store("posted", "channel")
	a.direct.Store("channel", "")
	endpoint := "ws" + strings.TrimPrefix(a.config.Origin, "http") + "/api/v3/bots/ws"
	conn, _, err := websocket.DefaultDialer.DialContext(t.Context(), endpoint, http.Header{"Authorization": []string{"Bearer test-token"}})
	require.NoError(t, err)
	var events []domain.InboundEvent
	done := make(chan error, 1)
	go func() {
		done <- a.connected(t.Context(), conn, func(ctx context.Context, event domain.InboundEvent) error {
			if event.SourceKey == "team:m:backfilled" {
				replayContext <- ctx
			} else {
				events = append(events, event)
			}
			return nil
		})
	}()
	select {
	case <-framesSent:
		replayCtx := <-replayContext
		select {
		case <-replayCtx.Done():
		case <-time.After(time.Second):
			t.Fatal("client did not observe disconnect")
		}
		close(releaseStamp)
	case <-time.After(time.Second):
		t.Fatal("frames blocked")
	}
	select {
	case err := <-done:
		require.ErrorIs(t, err, errReconnect)
	case <-time.After(time.Second):
		t.Fatal("connection did not end")
	}
	require.Len(t, events, 3)
	require.Equal(t, domain.InboundEventReaction, events[0].Kind)
	require.Equal(t, "changed", events[1].Text)
	require.True(t, events[2].Deleted)
}
