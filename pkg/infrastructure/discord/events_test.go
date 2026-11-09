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
	"sync"
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
	for _, mode := range []string{"top", "dm", "mention", "member", "owner", "starter", "reply", "thread starter", "stranger", "own"} {
		t.Run(mode, func(t *testing.T) {
			a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case "/channels/300":
					owner := "201"
					if mode == "owner" {
						owner = "100"
					}
					writeJSON(t, w, map[string]any{"id": "300", "type": 11, "parent_id": "400", "guild_id": "500", "owner_id": owner})
					return true
				case "/channels/300/thread-members/100":
					if mode == "member" {
						writeJSON(t, w, map[string]string{"id": "300", "user_id": "100"})
					} else {
						w.WriteHeader(404)
						writeJSON(t, w, map[string]any{"code": 10007})
					}
					return true
				case "/channels/400/messages/300":
					m := message(300, 400)
					if mode == "starter" || mode == "reply" || mode == "thread starter" {
						m.Author.ID = 100
					}
					writeJSON(t, w, m)
					return true
				}
				return false
			})
			m := message(600, 300)
			if mode == "reply" {
				m.Type = sdk.MessageTypeReply
			}
			if mode == "thread starter" {
				m.Type, m.Content = sdk.MessageTypeThreadStarterMessage, ""
			}
			if mode == "top" || mode == "own" {
				m.ChannelID = 400
			}
			if mode == "dm" {
				m.ChannelID = 900
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
			if mode == "stranger" || mode == "own" || mode == "thread starter" {
				require.Nil(t, e)
				return
			}
			require.NotNil(t, e)
			assert.Equal(t, "chat:m:600", e.SourceKey)
			assert.Equal(t, Conversation("chat", m.ChannelID.String(), "600"), e.MessageRef)
			assert.Equal(t, mode == "dm", e.Direct)
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
		assert.Equal(t, ":"+emoji+": on message 600", events[len(events)-1].Text)
		self = 200
		before := len(events)
		require.NoError(t, a.dispatch(t.Context(), e, consume))
		assert.Len(t, events, before)
	}
	require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageDeleteBulk{ChannelID: 400, IDs: []snowflake.ID{600, 601}}, consume))
	last := events[len(events)-1]
	assert.True(t, last.Deleted)
	assert.Empty(t, last.Text)
	assert.Equal(t, "200", last.Sender.ID)
	assert.Equal(t, "chat:d:600", last.SourceKey)
	a.SetConversationFilter(func(domain.ConversationRef, bool) bool { return false })
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

func TestBackfillIgnoresLocalEventTimes(t *testing.T) {
	for _, kind := range []string{"deletion", "reaction"} {
		t.Run(kind, func(t *testing.T) {
			old := message(600, 400)
			old.CreatedAt = time.Now().Add(-2 * time.Hour).UTC()
			missed := message(601, 400)
			missed.CreatedAt = old.CreatedAt.Add(time.Hour)
			a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/channels/400/messages" {
					writeJSON(t, w, []sdk.Message{missed})
					return true
				}
				return false
			})
			consume := a.history.SaveInbound
			require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageCreate{Message: old}, consume))
			var late gateway.EventData
			if kind == "deletion" {
				late = gateway.EventMessageDelete{ChannelID: 400, ID: 600}
			} else {
				self, emoji := snowflake.ID(100), "eyes"
				late = gateway.EventMessageReactionAdd{ChannelID: 400, MessageID: 700, UserID: 201, MessageAuthorID: &self, Emoji: sdk.PartialEmoji{Name: &emoji}}
			}
			require.NoError(t, a.dispatch(t.Context(), late, consume))
			last, err := a.history.ListInbound(t.Context(), "chat:400:", 1)
			require.NoError(t, err)
			require.Len(t, last, 1)
			require.True(t, last[0].ReceivedAt.After(missed.CreatedAt))
			events, err := a.backfillChannel(t.Context(), "chat:400:")
			require.NoError(t, err)
			require.Len(t, events, 1)
			assert.Equal(t, "chat:m:601", events[0].SourceKey)
		})
	}
}

type gatewaySession struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	closed chan struct{}
	pong   chan struct{}
}

func (s *gatewaySession) write(value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteJSON(value)
}
func (s *gatewaySession) dispatch(name string, value any) error {
	return s.write(map[string]any{"op": 0, "s": 2, "t": name, "d": value})
}

func fakeGateway(t *testing.T, a *adapter) <-chan *gatewaySession {
	t.Helper()
	sessions := make(chan *gatewaySession, 4)
	var conns sync.Map
	up := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conns.Store(conn, struct{}{})
		defer conns.Delete(conn)
		defer conn.Close()
		s := &gatewaySession{conn: conn, closed: make(chan struct{}), pong: make(chan struct{}, 8)}
		defer close(s.closed)
		// READY without HELLO isolates reconnect from Disgo's heartbeat race (disgoorg/disgo#494).
		conn.SetPongHandler(func(string) error { s.pong <- struct{}{}; return nil })
		if s.dispatch("READY", map[string]any{"v": 10, "session_id": "fake", "user": map[string]any{"id": "100", "username": "fake", "bot": true}, "guilds": []any{}}) != nil {
			return
		}
		sessions <- s
		for {
			var frame struct {
				Op int `json:"op"`
			}
			if conn.ReadJSON(&frame) != nil {
				return
			}
		}
	}))
	t.Cleanup(func() {
		conns.Range(func(key, value any) bool { _ = key.(*websocket.Conn).Close(); return true })
		server.Close()
	})
	a.gatewayOptions = []gateway.ConfigOpt{gateway.WithURL(strings.Replace(server.URL, "http:", "ws:", 1)), gateway.WithCompression(gateway.CompressionNone), gateway.WithEnableResumeURL(false)}
	a.createGateway = func(token string, listener gateway.EventHandlerFunc, options ...gateway.ConfigOpt) gateway.Gateway {
		g := gateway.New(token, listener, options...)
		// Close the reconnected client before the fake server to keep its retry logs out of later tests.
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			g.Close(ctx)
		})
		return g
	}
	return sessions
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
	queued := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			listener(nil, gateway.EventTypeMessageCreate, i+2, gateway.EventMessageCreate{Message: message(snowflake.ID(601+i), 400)})
		}
		close(queued)
	}()
	receive(t, queued)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, a.Shutdown(ctx), context.DeadlineExceeded)
	receive(t, closed)
	close(release)
	require.NoError(t, a.Shutdown(t.Context()))
	require.NoError(t, receive(t, done))
	require.Error(t, a.Start(t.Context(), func(context.Context, domain.InboundEvent) error { t.Fatal("consume after shutdown"); return nil }))
}

func TestShutdownDropsMessagesAfterDisgoReconnect(t *testing.T) {
	a := testAdapter(t, Config{}, nil)
	sessions := fakeGateway(t, a)
	consumed := make(chan domain.InboundEvent, 4)
	done := make(chan error, 1)
	go func() {
		done <- a.Start(t.Context(), func(_ context.Context, e domain.InboundEvent) error { consumed <- e; return nil })
	}()
	s := receive(t, sessions)
	require.NoError(t, s.dispatch("MESSAGE_CREATE", message(600, 400)))
	receive(t, consumed)
	require.NoError(t, s.write(map[string]any{"op": 7, "d": nil}))
	receive(t, s.closed)
	require.NoError(t, a.Shutdown(t.Context()))
	require.NoError(t, receive(t, done))
	late := receive(t, sessions)
	require.NoError(t, late.dispatch("MESSAGE_CREATE", message(601, 400)))
	require.NoError(t, late.conn.WriteControl(websocket.PingMessage, []byte("barrier"), time.Now().Add(time.Second)))
	receive(t, late.pong)
	select {
	case e := <-consumed:
		t.Fatalf("consume after shutdown: %+v", e)
	default:
	}
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

func TestStartRetriesAfterFailure(t *testing.T) {
	a := testAdapter(t, Config{}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	a.createGateway = func(_ string, _ gateway.EventHandlerFunc, _ ...gateway.ConfigOpt) gateway.Gateway {
		calls++
		if calls == 1 {
			return &stubGateway{openErr: errors.New("temporary connection failure")}
		}
		cancel()
		return &stubGateway{}
	}
	consume := func(context.Context, domain.InboundEvent) error { return nil }
	require.ErrorContains(t, a.Start(ctx, consume), "connect Discord gateway failed")
	require.NoError(t, a.Start(ctx, consume))
	assert.Equal(t, 2, calls)
}

func TestLiveAndBackfillCollapseInInbox(t *testing.T) {
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
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "dedup.db")})
	require.NoError(t, err)
	defer db.Close()
	a.history = repository.NewEventRepository(db)
	inbox := repository.NewInboxRepository(db)
	consume := func(ctx context.Context, e domain.InboundEvent) error {
		accepted := true
		e.Accepted = &accepted
		return inbox.Receive(ctx, e, domain.InboxItem{SourceKey: e.SourceKey, SessionKey: "chat:400:", Lane: "conv", ReadyAt: e.ReceivedAt, Payload: domain.InboxPayload{Event: &e}})
	}
	require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageCreate{Message: m}, consume))
	require.NoError(t, a.backfill(t.Context(), consume))
	require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageCreate{Message: m}, consume))
	require.NoError(t, a.dispatch(t.Context(), gateway.EventMessageUpdate{Message: m}, consume))
	require.NoError(t, a.backfill(t.Context(), consume))
	events, err := a.history.ListInbound(t.Context(), "chat:400:", 100)
	require.NoError(t, err)
	assert.Len(t, events, 2)
	items, err := inbox.Active(t.Context())
	require.NoError(t, err)
	assert.Len(t, items, 2)
}
