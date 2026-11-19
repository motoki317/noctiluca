package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	sdk "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/infrastructure/slack"
)

type adapterNotesMessaging struct {
	*namedMessaging
	adapter domain.MessagingAdapter
}

func (m *adapterNotesMessaging) ToolNotes(ctx context.Context) string {
	return m.adapter.ToolNotes(ctx)
}

func TestMainSessionNotes(t *testing.T) {
	router, err := newMessagingRouter([]Messaging{{Name: "entry", Adapter: newNamedMessaging("entry")}})
	require.NoError(t, err)
	notes := (&core{messaging: router}).toolNotes(t.Context())
	for _, rule := range []string{
		"Only this session receives inputs and posts.",
		"Post content from one conversation in another only when that audience is allowed to see it.",
		"Long context lowers the quality of your work without your noticing. When an input includes a Context line, call the compact tool at your next break: " +
			"nothing you started in this context is half-done, and every reply it owes is posted. Background tasks and subagents may keep running. " +
			"Compaction runs after the turn ends. Do not rest for this.\n\nDelegation:",
		"- Never wait on a subagent.",
	} {
		require.Contains(t, notes, rule)
	}
	require.Contains(t, notes, "\n\nentry:\nUse entry_send with native IDs.")
	for _, name := range []string{"Claude", "Codex", "Slack", "Discord", "traQ", "spawn_agent", "SendMessage"} {
		require.NotContains(t, notes, name)
	}
}

func TestOwnerDMFailureDoesNotBlockPrompt(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/conversations.open", r.URL.Path)
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"ok":false,"error":"missing_scope","needed":"im:write"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"channel":{"id":"D_OWNER"}}`)
	}))
	t.Cleanup(server.Close)
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "notes.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	adapter, err := slack.New("chat", slack.Config{AppToken: "app", BotToken: "bot", Owner: "owner"}, repository.NewEventRepository(db), sdk.OptionAPIURL(server.URL+"/"))
	require.NoError(t, err)
	m := &adapterNotesMessaging{namedMessaging: newNamedMessaging("chat"), adapter: adapter}
	f := setup(t, Config{}, []Messaging{{Name: "chat", Adapter: m, Owner: "owner", Rules: map[domain.ConversationRef]domain.AcceptRule{
		"chat:first": {Users: []string{"owner"}}, "chat:second": {Users: []string{"owner"}}, "chat:third": {Users: []string{"owner"}},
	}}}, fixtureOptions{})
	for i, text := range []string{"first", "second", "third"} {
		m.in <- message(domain.ConversationRef("chat:"+text), "owner", text)
		model := take(t, f.model.opened)
		require.Contains(t, take(t, model.inputs).Text, text)
		require.Contains(t, model.config.SystemPrompt, "Use chat_send to post text.")
		if i == 0 {
			require.NotContains(t, model.config.SystemPrompt, "Owner DM:")
		} else {
			require.Contains(t, model.config.SystemPrompt, "Owner DM: use chat_send with channel=D_OWNER and no thread_ts.")
		}
		_, err := f.hub.tools["rest"].Handler(domain.WithToolSession(t.Context(), domain.ToolSession{Key: domain.MainSessionKey}), nil)
		require.NoError(t, err)
		model.idle.Store(true)
		model.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
		take(t, model.closed)
	}
	require.EqualValues(t, 2, calls.Load())
}
