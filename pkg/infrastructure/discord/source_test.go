package discord

import (
	"context"
	"net/http"
	"testing"
	"time"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func TestProviders(t *testing.T) {
	pages := 0
	a := testAdapter(t, Config{NeedGroups: true}, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/users/@me/guilds":
			writeJSON(t, w, []map[string]string{{"id": "500"}})
			return true
		case "/guilds/500/members":
			pages++
			assert.Equal(t, "1000", r.URL.Query().Get("limit"))
			if r.URL.Query().Get("after") == "0" {
				var members []sdk.Member
				for i := 1; i <= 1000; i++ {
					members = append(members, sdk.Member{User: sdk.User{ID: snowflake.ID(i)}, RoleIDs: []snowflake.ID{700}})
				}
				writeJSON(t, w, members)
			} else {
				assert.Equal(t, "1000", r.URL.Query().Get("after"))
				writeJSON(t, w, []sdk.Member{{User: sdk.User{ID: 1001}, RoleIDs: []snowflake.ID{701}}, {User: sdk.User{ID: 1002}}})
			}
			return true
		case "/guilds/500/members/200":
			writeJSON(t, w, map[string]any{"user": map[string]string{"id": "200", "username": "reader"}, "nick": "Nickname"})
			return true
		case "/channels/400/messages/300":
			writeJSON(t, w, message(300, 400))
			return true
		}
		return false
	})
	dm, err := a.DirectMessage(t.Context(), "200")
	require.NoError(t, err)
	assert.Equal(t, domain.ConversationRef("chat:900:"), dm)
	info := a.SourceInfo(t.Context(), "chat:400:300", "200", true)
	assert.Equal(t, "#room", info.Conversation)
	assert.Equal(t, "Nickname", info.Sender)
	assert.Equal(t, "hello", info.ThreadText)
	groups, err := a.UserGroups(t.Context(), []string{"700", "701", "702", "500"})
	require.NoError(t, err)
	assert.Len(t, groups["700"], 1000)
	assert.Equal(t, []string{"1001"}, groups["701"])
	assert.Empty(t, groups["702"])
	assert.Len(t, groups["500"], 1002)
	assert.Equal(t, 2, pages)
	groups, err = a.UserGroups(t.Context(), nil)
	require.NoError(t, err)
	assert.Empty(t, groups)
	assert.Equal(t, 2, pages)
}

func TestGatewayIntentsAndQueue(t *testing.T) {
	for _, groups := range []bool{false, true} {
		t.Run(map[bool]string{false: "without_roles", true: "with_roles"}[groups], func(t *testing.T) {
			a := testAdapter(t, Config{NeedGroups: groups}, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			created := false
			a.createGateway = func(token string, listener gateway.EventHandlerFunc, opts ...gateway.ConfigOpt) gateway.Gateway {
				created = true
				g := gateway.New(token, listener, opts...)
				cancel()
				for _, intent := range []gateway.Intents{gateway.IntentGuilds, gateway.IntentGuildMessages, gateway.IntentGuildMessageReactions, gateway.IntentDirectMessages, gateway.IntentDirectMessageReactions, gateway.IntentMessageContent} {
					assert.NotZero(t, g.Intents()&intent)
				}
				assert.Equal(t, groups, g.Intents()&gateway.IntentGuildMembers != 0)
				return &stubGateway{}
			}
			require.NoError(t, a.Start(ctx, func(context.Context, domain.InboundEvent) error { t.Fatal("canceled consumption"); return nil }))
			assert.True(t, created)
		})
	}
	q := eventQueue{ready: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	for i := 1; i <= 10000; i++ {
		q.push(ctx, gateway.EventMessageDelete{ID: snowflake.ID(i)})
	}
	receive(t, q.ready)
	events := q.take()
	require.Len(t, events, 10000)
	for i, e := range events {
		assert.Equal(t, snowflake.ID(i+1), e.(gateway.EventMessageDelete).ID)
	}
	cancel()
	q.push(ctx, gateway.EventMessageDelete{ID: 10001})
	assert.Empty(t, q.take())
}

func TestBackfillAgeAndResume(t *testing.T) {
	var histories int
	a := testAdapter(t, Config{Channels: []string{"400"}}, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/channels/400/messages" {
			return false
		}
		histories++
		old := message(600, 400)
		old.CreatedAt = time.Now().Add(-25 * time.Hour)
		fresh := message(601, 400)
		fresh.CreatedAt = time.Now().Add(-time.Hour)
		edited := fresh.CreatedAt.Add(time.Minute)
		fresh.EditedTimestamp = &edited
		writeJSON(t, w, []sdk.Message{fresh, old})
		return true
	})
	var events []domain.InboundEvent
	consume := func(_ context.Context, e domain.InboundEvent) error { events = append(events, e); return nil }
	require.NoError(t, a.dispatch(t.Context(), gateway.EventResumed{}, consume))
	require.Len(t, events, 2)
	assert.Equal(t, "chat:m:601", events[0].SourceKey)
	assert.Equal(t, domain.InboundEventMessageEdited, events[1].Kind)
	require.NoError(t, a.dispatch(t.Context(), gateway.EventResumed{}, consume))
	assert.Equal(t, 2, histories)
	assert.Equal(t, events[0].SourceKey, events[2].SourceKey)
}

func TestBackfillRetryDoesNotSkipMessagesBeforeAnEdit(t *testing.T) {
	old := message(600, 400)
	old.CreatedAt = time.Now().Add(-2 * time.Hour).UTC()
	newer := message(601, 400)
	newer.CreatedAt = old.CreatedAt.Add(time.Hour)
	edit := newer.CreatedAt.Add(time.Minute)
	old.EditedTimestamp = &edit
	a := testAdapter(t, Config{Channels: []string{"400"}}, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/channels/400/messages" {
			return false
		}
		writeJSON(t, w, []sdk.Message{newer, old})
		return true
	})
	calls := 0
	err := a.backfill(t.Context(), func(ctx context.Context, e domain.InboundEvent) error {
		calls++
		if calls == 2 {
			return context.Canceled
		}
		return a.history.SaveInbound(ctx, e)
	})
	require.ErrorIs(t, err, context.Canceled)
	var keys []string
	require.NoError(t, a.backfill(t.Context(), func(ctx context.Context, e domain.InboundEvent) error {
		keys = append(keys, e.SourceKey)
		return a.history.SaveInbound(ctx, e)
	}))
	assert.Equal(t, []string{"chat:m:600", "chat:m:601", "chat:e:600:" + edit.Format(time.RFC3339Nano)}, keys)
}

func TestDirectReactionsToHumanMessages(t *testing.T) {
	a := testAdapter(t, Config{}, nil)
	emoji := "👍"
	human := snowflake.ID(200)
	e, err := a.reaction(t.Context(), gateway.EventMessageReactionAdd{UserID: 201, ChannelID: 900, MessageID: 600, MessageAuthorID: &human, Emoji: sdk.PartialEmoji{Name: &emoji}})
	require.NoError(t, err)
	require.NotNil(t, e)
	assert.True(t, e.Direct)
}
