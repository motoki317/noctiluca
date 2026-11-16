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
	}
	require.EqualValues(t, 2, calls.Load())
}
