package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func testAdapter(t *testing.T, config Config, handle func(http.ResponseWriter, *http.Request) bool) *adapter {
	t.Helper()
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "events.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bot fake-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if handle != nil && handle(w, r) {
			return
		}
		switch {
		case r.URL.Path == "/users/@me/channels":
			writeJSON(t, w, map[string]any{"id": "900", "type": 1})
		case strings.HasSuffix(r.URL.Path, "/messages"):
			writeJSON(t, w, []any{})
		case strings.HasPrefix(r.URL.Path, "/channels/"):
			id := strings.TrimPrefix(r.URL.Path, "/channels/")
			typ := 0
			if id == "900" {
				typ = 1
			}
			writeJSON(t, w, map[string]any{"id": id, "type": typ, "name": "room", "guild_id": "500"})
		case strings.HasPrefix(r.URL.Path, "/users/"):
			writeJSON(t, w, map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/users/"), "username": "reader"})
		default:
			t.Errorf("unexpected REST request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	config.Token = "fake-token"
	config.Owner = "200"
	a, err := newAdapter("chat", config, repository.NewEventRepository(db), rest.WithURL(server.URL), rest.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	a.self.Store(100)
	t.Cleanup(func() { require.NoError(t, a.Shutdown(context.Background())) })
	return a
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	require.NoError(t, json.NewEncoder(w).Encode(value))
}
func message(id, channel snowflake.ID) sdk.Message {
	return sdk.Message{ID: id, ChannelID: channel, Author: sdk.User{ID: 200, Username: "reader"}, Content: "hello", CreatedAt: time.Now().UTC()}
}

func seedConversations(t *testing.T, a *adapter, channels ...string) {
	t.Helper()
	for _, channel := range channels {
		accepted := true
		require.NoError(t, a.history.SaveInbound(t.Context(), domain.InboundEvent{
			Kind: domain.InboundEventMessage, Conversation: Conversation(a.name, channel, ""), Accepted: &accepted,
			Sender: domain.Sender{ID: "200", Kind: domain.SenderHuman}, Text: "stored message", ReceivedAt: time.Now().Add(-25 * time.Hour),
		}))
	}
}

func TestMessagesAndThreadParticipation(t *testing.T) {
	for _, mode := range []string{"mention", "reply", "thread starter", "own"} {
		t.Run(mode, func(t *testing.T) {
			a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/channels/300" {
					writeJSON(t, w, map[string]any{"id": "300", "type": 11, "parent_id": "400", "guild_id": "500", "owner_id": "201"})
					return true
				}
				return false
			})
			m := message(600, 300)
			if mode == "reply" {
				m.Type = sdk.MessageTypeReply
				m.ChannelID = 400
			}
			if mode == "thread starter" {
				m.Type, m.Content = sdk.MessageTypeThreadStarterMessage, ""
			}
			if mode == "own" {
				m.ChannelID = 400
			}
			if mode == "mention" {
				m.Mentions = []sdk.User{{ID: 100}}
			}
			if mode == "own" {
				m.Author.ID = 100
			}
			if mode != "thread starter" {
				m.Attachments = []sdk.Attachment{{ID: 700, Filename: "sample.txt"}}
			}
			e, err := a.message(t.Context(), m, false)
			require.NoError(t, err)
			if mode == "own" || mode == "thread starter" {
				require.Nil(t, e)
				return
			}
			require.NotNil(t, e)
			assert.Equal(t, "chat:m:600", e.SourceKey)
			assert.Equal(t, Conversation("chat", m.ChannelID.String(), "600"), e.MessageRef)
			assert.False(t, e.Direct)
			assert.Equal(t, mode == "mention", e.MentionsMe)
			if m.ChannelID == 300 {
				assert.Equal(t, domain.ConversationRef("chat:400:300"), e.Conversation)
				assert.Equal(t, domain.ConversationRef("chat:400:"), e.Parent)
			}
			assert.Equal(t, fmt.Sprintf("chat:%s:600:700", m.ChannelID), e.Attachments[0].Ref)
		})
	}
}

func TestEditsDeletesAndReactions(t *testing.T) {
	updated := message(600, 400)
	at := time.Now().UTC()
	updated.EditedTimestamp = &at
	updated.Content = "edited"
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/channels/400/messages/600" {
			writeJSON(t, w, updated)
			return true
		}
		return false
	})
	var events []domain.InboundEvent
	consume := func(ctx context.Context, e domain.InboundEvent) error {
		events = append(events, e)
		return a.history.SaveInbound(ctx, e)
	}
	require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageUpdate{Message: sdk.Message{ID: 600, ChannelID: 400}}, consume))
	require.Empty(t, events)
	require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageUpdate{Message: sdk.Message{ID: 600, ChannelID: 400, EditedTimestamp: &at}}, consume))
	require.Len(t, events, 1)
	assert.Equal(t, domain.InboundEventMessageEdited, events[0].Kind)
	assert.Equal(t, "edited", events[0].Text)
	assert.Equal(t, "chat:e:600:"+at.Format(time.RFC3339Nano), events[0].SourceKey)
	for _, emoji := range []string{"👍", "dislike"} {
		self := snowflake.ID(100)
		e := gateway.EventMessageReactionAdd{UserID: 201, ChannelID: 400, MessageID: 600, MessageAuthorID: &self, Emoji: sdk.PartialEmoji{Name: &emoji}}
		require.NoError(t, a.dispatch(t.Context(), e, consume))
		assert.Equal(t, domain.InboundEventReaction, events[len(events)-1].Kind)
		assert.Equal(t, ":"+emoji+": on message 600", events[len(events)-1].Text)
		self = 200
		before := len(events)
		require.NoError(t, a.dispatch(t.Context(), e, consume))
		assert.Len(t, events, before)
	}
	require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageDeleteBulk{ChannelID: 400, IDs: []snowflake.ID{600, 601}}, consume))
	last := events[len(events)-1]
	assert.Equal(t, domain.InboundEventMessageEdited, last.Kind)
	assert.True(t, last.Deleted)
	assert.Empty(t, last.Text)
	assert.Equal(t, "200", last.Sender.ID)
	assert.Equal(t, "chat:d:600", last.SourceKey)
	a.SetConversationFilter(func(domain.ConversationRef) bool { return false })
	self := snowflake.ID(100)
	emoji := "x"
	before := len(events)
	require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageReactionAdd{UserID: 201, ChannelID: 400, MessageID: 600, MessageAuthorID: &self, Emoji: sdk.PartialEmoji{Name: &emoji}}, consume))
	assert.Len(t, events, before)
}

func TestBackfillCursorCapsAndKeys(t *testing.T) {
	now := time.Now().UTC()
	var pages int
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/channels/400/messages" {
			return false
		}
		pages++
		assert.Equal(t, "100", r.URL.Query().Get("limit"))
		offset := 0
		if r.URL.Query().Get("before") != "" {
			offset = 100
		}
		var ms []sdk.Message
		for i := 0; i < 100; i++ {
			at := now.Add(-time.Duration(offset+i) * time.Minute)
			m := message(snowflake.New(at), 400)
			m.CreatedAt = at
			ms = append(ms, m)
		}
		writeJSON(t, w, ms)
		return true
	})
	seedConversations(t, a, "400", "400")
	var got []domain.InboundEvent
	consume := func(_ context.Context, e domain.InboundEvent) error { got = append(got, e); return nil }
	require.NoError(t, a.backfill(t.Context(), consume))
	require.Len(t, got, 200)
	assert.Equal(t, 2, pages)
	assert.True(t, got[0].ReceivedAt.Before(got[199].ReceivedAt))
	live, err := a.message(t.Context(), message(snowflake.New(now), 400), false)
	require.NoError(t, err)
	assert.Equal(t, live.SourceKey, got[199].SourceKey)
	require.NoError(t, a.history.SaveInbound(t.Context(), got[198]))
	got = nil
	pages = 0
	require.NoError(t, a.backfill(t.Context(), consume))
	assert.Len(t, got, 2)
	assert.Equal(t, 2, pages)
	stop := errors.New("inbox unavailable")
	require.ErrorIs(t, a.backfill(t.Context(), func(context.Context, domain.InboundEvent) error { return stop }), stop)
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}

func TestShutdownJoinsConsumption(t *testing.T) {
	a := testAdapter(t, Config{}, nil)
	listeners := make(chan gateway.EventHandlerFunc, 1)
	closed := make(chan struct{})
	a.createGateway = func(_ string, listener gateway.EventHandlerFunc, _ ...gateway.ConfigOpt) gateway.Gateway {
		listeners <- listener
		return &stubGateway{onClose: func() { close(closed) }}
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.Start(t.Context(), func(ctx context.Context, _ domain.InboundEvent) error {
			close(entered)
			<-ctx.Done()
			<-release
			return nil
		})
	}()
	listener := receive(t, listeners)
	listener(nil, gateway.EventTypeMessageCreate, 1, gateway.EventMessageCreate{Message: message(600, 400)})
	receive(t, entered)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, a.Shutdown(ctx), context.DeadlineExceeded)
	receive(t, closed)
	close(release)
	require.NoError(t, a.Shutdown(t.Context()))
	require.NoError(t, receive(t, done))
	require.Error(t, a.Start(t.Context(), func(context.Context, domain.InboundEvent) error { t.Fatal("consume after shutdown"); return nil }))
}

type checkedGatewayLimiter struct {
	gateway.RateLimiter
	check func()
}

func (l checkedGatewayLimiter) Wait(ctx context.Context, kind gateway.RateLimiterCommandType) error {
	l.check()
	return l.RateLimiter.Wait(ctx, kind)
}

func TestGatewayStartShutdown(t *testing.T) {
	a := testAdapter(t, Config{}, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		// READY without HELLO avoids Disgo's heartbeat race (disgoorg/disgo#494).
		require.NoError(t, conn.WriteJSON(map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{
			"v": 10, "session_id": "fake", "user": map[string]any{"id": "100", "username": "fake", "bot": true}, "guilds": []any{},
		}}))
		require.NoError(t, conn.WriteJSON(map[string]any{"op": 0, "s": 2, "t": "MESSAGE_CREATE", "d": message(600, 400)}))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	a.gatewayOptions = []gateway.ConfigOpt{gateway.WithURL("ws" + strings.TrimPrefix(server.URL, "http")), gateway.WithCompression(gateway.CompressionNone)}
	var g gateway.Gateway
	a.createGateway = func(token string, handler gateway.EventHandlerFunc, opts ...gateway.ConfigOpt) gateway.Gateway {
		limiter := checkedGatewayLimiter{RateLimiter: gateway.NewRateLimiter(), check: func() {
			// Send must leave status unlocked while it holds the connection lock (disgoorg/disgo#646).
			status := make(chan gateway.Status, 1)
			go func() { status <- g.Status() }()
			select {
			case <-status:
			case <-time.After(time.Second):
				t.Error("Send blocks gateway status during shutdown")
			}
		}}
		g = gateway.New(token, handler, append(opts, gateway.WithRateLimiter(limiter))...)
		return g
	}
	consumed := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- a.Start(t.Context(), func(context.Context, domain.InboundEvent) error { consumed <- struct{}{}; return nil })
	}()
	receive(t, consumed)
	require.NoError(t, g.Send(t.Context(), gateway.OpcodePresenceUpdate, gateway.MessageDataPresenceUpdate{}))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, a.Shutdown(ctx))
	require.NoError(t, receive(t, done))
}

type stubGateway struct {
	gateway.Gateway
	openErr error
	onOpen  func(context.Context) error
	status  func() gateway.Status
	onClose func()
}

func (s *stubGateway) Open(ctx context.Context) error {
	if s.onOpen != nil {
		return s.onOpen(ctx)
	}
	return s.openErr
}
func (s *stubGateway) Status() gateway.Status {
	if s.status != nil {
		return s.status()
	}
	return gateway.StatusReady
}
func (s *stubGateway) Close(context.Context) {
	if s.onClose != nil {
		s.onClose()
	}
}

func TestLiveAndBackfillShareEditKey(t *testing.T) {
	m := message(600, 400)
	edit := m.CreatedAt.Add(time.Second)
	m.EditedTimestamp = &edit
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/channels/400/messages":
			writeJSON(t, w, []sdk.Message{m})
			return true
		case "/channels/400/messages/600":
			writeJSON(t, w, m)
			return true
		}
		return false
	})
	var live domain.InboundEvent
	require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageUpdate{Message: m}, func(_ context.Context, e domain.InboundEvent) error {
		live = e
		return nil
	}))
	events, err := a.backfillChannel(t.Context(), "chat:400:")
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.NotEmpty(t, live.SourceKey)
	assert.Equal(t, live.SourceKey, events[1].SourceKey)
}
