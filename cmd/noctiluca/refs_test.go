package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/discord"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/infrastructure/slack"
	"github.com/motoki317/noctiluca/pkg/infrastructure/traq"
)

func TestAdapterPostRefs(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	defer db.Close()
	events := repository.NewEventRepository(db)
	for _, platform := range []struct {
		name string
		new  func(domain.EventRepository) (domain.MessagingAdapter, error)
	}{
		{"chat", func(events domain.EventRepository) (domain.MessagingAdapter, error) {
			return slack.New("chat", slack.Config{AppToken: "fake", BotToken: "fake"}, events)
		}},
		{"community", func(events domain.EventRepository) (domain.MessagingAdapter, error) {
			return discord.New("community", discord.Config{Token: "fake", Owner: "1"}, events)
		}},
		{"team", func(events domain.EventRepository) (domain.MessagingAdapter, error) {
			return traq.New("team", traq.Config{Origin: "https://example.com", AccessToken: "fake"}, events)
		}},
	} {
		t.Run(platform.name, func(t *testing.T) {
			adapter, err := platform.new(events)
			require.NoError(t, err)
			defer func() { require.NoError(t, adapter.Shutdown(t.Context())) }()
			provider, ok := adapter.(domain.PostRefProvider)
			require.True(t, ok)
			for _, tc := range []struct {
				name, thread, message string
				direct, want          bool
			}{
				{name: "channel", message: "400", want: true},
				{name: "thread", thread: "300", message: "400"},
				{name: "DM", message: "400", direct: true},
				{name: "no message"},
			} {
				if platform.name == "team" && tc.name == "thread" {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					conversation := domain.NewConversationRef(platform.name, "200", tc.thread)
					var message domain.ConversationRef
					if tc.message != "" {
						message = domain.NewConversationRef(platform.name, "200", tc.message)
					}
					var want []domain.NamedRef
					if tc.want {
						name := "thread"
						if platform.name == "team" {
							name = "quote"
						}
						want = []domain.NamedRef{{Name: name, Ref: message}}
					}
					require.Equal(t, want, provider.PostRefs(conversation, message, tc.direct))
				})
			}
		})
	}
}
