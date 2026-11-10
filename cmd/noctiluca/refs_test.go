package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/discord"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/infrastructure/slack"
	"github.com/motoki317/noctiluca/pkg/infrastructure/traq"
	"github.com/motoki317/noctiluca/pkg/usecase/core"
	"github.com/motoki317/noctiluca/pkg/usecase/reconcile"
	"github.com/motoki317/noctiluca/pkg/usecase/toolhub"
)

type notesMessaging struct{ domain.MessagingAdapter }

func (notesMessaging) Start(ctx context.Context, _ func(context.Context, domain.InboundEvent) error) error {
	<-ctx.Done()
	return nil
}

type notesModel struct{ prompts chan string }

func (m notesModel) Open(_ context.Context, config domain.ModelSessionConfig) (domain.ModelSession, error) {
	m.prompts <- config.SystemPrompt
	return nil, errors.New("prompt captured")
}

type notesHub struct {
	domain.ToolHub
	tools []domain.Tool
}

func (h *notesHub) AddSource(name string, tools []domain.Tool) error {
	h.tools = append(h.tools, tools...)
	return h.ToolHub.AddSource(name, tools)
}

func TestRefVocabulary(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	events := repository.NewEventRepository(db)
	a, err := slack.New("chat", slack.Config{AppToken: "fake", BotToken: "fake"}, events)
	require.NoError(t, err)
	b, err := discord.New("community", discord.Config{Token: "fake", Owner: "1"}, events)
	require.NoError(t, err)
	c, err := traq.New("team", traq.Config{Origin: "https://example.com", AccessToken: "fake"}, events)
	require.NoError(t, err)
	hub, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	h := &notesHub{ToolHub: hub}
	inbox := repository.NewInboxRepository(db)
	model := notesModel{prompts: make(chan string, 1)}
	agent, err := core.New(core.Config{}, []core.Messaging{
		{Name: "chat", Adapter: notesMessaging{a}},
		{Name: "community", Adapter: notesMessaging{b}},
		{Name: "team", Adapter: notesMessaging{c}},
	}, model, h, events, repository.NewSessionRepository(db), nil, repository.NewScheduleRepository(db), repository.NewWatchRepository(db), nil, nil, inbox)
	require.NoError(t, err)
	_, err = inbox.Enqueue(t.Context(), reconcile.TaskItem("notes", "check notes", t.TempDir()))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- agent.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	var prompt string
	select {
	case prompt = <-model.prompts:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not receive the notes")
	}
	texts := []string{prompt}
	for _, tool := range h.tools {
		texts = append(texts, tool.Description, string(tool.InputSchema))
	}
	for _, words := range [][]string{{"work", "target"}, {"reply", "target"}, {"reply", "→"}, {"work", "→"}} {
		for _, text := range texts {
			require.NotContains(t, text, strings.Join(words, " "))
		}
	}
}

type postRefsMessaging struct {
	notesMessaging
	domain.PostRefProvider
}

type refsModel struct{ inputs chan domain.UserInput }

func (m refsModel) Open(context.Context, domain.ModelSessionConfig) (domain.ModelSession, error) {
	return refsSession(m), nil
}

type refsSession struct{ inputs chan domain.UserInput }

func (s refsSession) Send(_ context.Context, input domain.UserInput) error {
	s.inputs <- input
	return errors.New("input captured")
}
func (refsSession) Events() <-chan domain.ModelEvent { return nil }
func (refsSession) ResumeToken() string              { return "" }
func (refsSession) Idle() bool                       { return true }
func (refsSession) Close() error                     { return nil }

func TestAdapterPostRefHeaders(t *testing.T) {
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
		for _, tc := range []struct {
			name, final, message string
			direct, want         bool
		}{
			{name: "channel", message: "400", want: true},
			{name: "thread", final: "300", message: "400"},
			{name: "DM", message: "400", direct: true},
			{name: "no message"},
		} {
			if platform.name == "team" && tc.name == "thread" {
				continue
			}
			for _, source := range []string{"event", "continuation"} {
				t.Run(platform.name+"/"+tc.name+"/"+source, func(t *testing.T) {
					dir := t.TempDir()
					db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(dir, "db")})
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					events := repository.NewEventRepository(db)
					adapter, err := platform.new(events)
					require.NoError(t, err)
					provider, ok := adapter.(domain.PostRefProvider)
					require.True(t, ok)
					hub, err := toolhub.New(toolhub.Config{})
					require.NoError(t, err)
					inbox := repository.NewInboxRepository(db)
					model := refsModel{inputs: make(chan domain.UserInput, 1)}
					agent, err := core.New(core.Config{}, []core.Messaging{{Name: platform.name, Owner: "owner", Adapter: postRefsMessaging{notesMessaging{adapter}, provider}}}, model, hub, events, repository.NewSessionRepository(db), nil, repository.NewScheduleRepository(db), repository.NewWatchRepository(db), nil, nil, inbox)
					require.NoError(t, err)
					channel := domain.NewConversationRef(platform.name, "200", "")
					conversation := domain.NewConversationRef(platform.name, "200", tc.final)
					var message domain.ConversationRef
					if tc.message != "" {
						message = domain.NewConversationRef(platform.name, "200", tc.message)
					}
					item := domain.InboxItem{SourceKey: "input", SessionKey: domain.SessionKey(channel), Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{
						Target: conversation, MessageRef: message, Context: domain.SessionContext{Conversation: channel, Direct: tc.direct, Workdir: dir},
					}}
					if source == "event" {
						item.Payload.Event = &domain.InboundEvent{Kind: domain.InboundEventMessage, Conversation: conversation, MessageRef: message, Direct: tc.direct,
							Sender: domain.Sender{ID: "owner", Kind: domain.SenderHuman}, Text: "input", ReceivedAt: item.ReadyAt}
						if tc.final != "" {
							item.Payload.Event.Parent = channel
						}
					}
					_, err = inbox.Enqueue(t.Context(), item)
					require.NoError(t, err)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					done := make(chan error, 1)
					go func() { done <- agent.Start(ctx) }()
					t.Cleanup(func() {
						cancel()
						require.NoError(t, <-done)
					})
					select {
					case input := <-model.inputs:
						_, header, found := strings.Cut(input.Text, "\nrefs: ")
						require.True(t, found)
						line, _, _ := strings.Cut(header, "\n")
						want := "conversation=" + string(conversation)
						if tc.want {
							name := "thread"
							if platform.name == "team" {
								name = "quote"
							}
							want += " " + name + "=" + string(message)
						}
						require.Equal(t, want, line)
					case <-time.After(5 * time.Second):
						t.Fatal("model did not receive the input")
					}
				})
			}
		}
	}
}

func TestRunInterruptedWorkPlacement(t *testing.T) {
	for _, tc := range []struct {
		name, channel, thread, wantThread string
		direct                            bool
	}{
		{"channel", "C_ROOM", "", "1.2", false},
		{"thread", "C_ROOM", "1.0", "1.0", false},
		{"DM", "D_OWNER", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binary, err := os.Executable()
			require.NoError(t, err)
			t.Setenv("NOCTILUCA_RUN_TEST_BINARY", binary)
			t.Setenv("NOCTILUCA_RUN_TEST_DIR", dir)
			t.Setenv("NOCTILUCA_RUN_RECOVERY", "1")
			t.Setenv("NOCTILUCA_RUN_WORK", "1")
			t.Setenv("TEST_APP_TOKEN", "xapp-fake")
			t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexec \"$NOCTILUCA_RUN_TEST_BINARY\" -test.run=TestRunCLIHelper -- \"$@\"\n"), 0700))
			path := filepath.Join(dir, "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte("messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\n    slack:\n      app_token: {env: TEST_APP_TOKEN}\n      bot_token: {env: TEST_BOT_TOKEN}\nmodel: {executable: ./claude}\nheartbeat: {enabled: false}\n"), 0600))
			config, err := loadConfig(path)
			require.NoError(t, err)
			api := newRunSlack(t, nil)
			app, err := newApplication(t.Context(), config, nil, sdk.OptionAPIURL(api.URL+"/"))
			require.NoError(t, err)
			channel := slack.Conversation("slack", tc.channel, "")
			conversation := slack.Conversation("slack", tc.channel, tc.thread)
			message := slack.Conversation("slack", tc.channel, "1.2")
			info := domain.SessionContext{Conversation: channel, Direct: tc.direct, Workdir: dir}
			event := &domain.InboundEvent{Conversation: conversation, MessageRef: message, Direct: tc.direct}
			if tc.thread != "" {
				event.Parent = channel
			}
			inbox := repository.NewInboxRepository(app.db)
			key := domain.SessionKey(channel)
			id, err := inbox.Enqueue(t.Context(), domain.InboxItem{SourceKey: "opening", SessionKey: key, Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Context: info, Event: event}})
			require.NoError(t, err)
			claimed, err := inbox.Claim(t.Context(), id, time.Now().Add(time.Hour))
			require.NoError(t, err)
			require.True(t, claimed)
			require.NoError(t, inbox.Delivered(t.Context(), id, domain.Session{Key: key, ResumeToken: "run-session", Context: &info, Turn: &domain.SessionTurn{InputID: id, Deadline: time.Now().Add(time.Hour)}}))
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- app.Start(ctx) }()
			t.Cleanup(func() {
				cancel()
				require.NoError(t, <-done)
			})
			select {
			case post := <-api.posts:
				require.Equal(t, tc.channel, post.Get("channel"))
				require.Equal(t, tc.wantThread, post.Get("thread_ts"))
			case <-time.After(5 * time.Second):
				t.Fatal("continuation did not post")
			}
			data, err := os.ReadFile(filepath.Join(dir, "recovery.jsonl"))
			require.NoError(t, err)
			var record struct{ Input string }
			require.NoError(t, json.Unmarshal(data, &record))
			refs := "\nrefs: conversation=" + string(conversation)
			if tc.name == "channel" {
				refs += " thread=" + string(message)
			}
			require.Contains(t, record.Input, refs+"\n\n")
			require.NotContains(t, record.Input, " message=")
		})
	}
}
