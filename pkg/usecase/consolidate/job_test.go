package consolidate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/usecase/reconcile"
	"github.com/stretchr/testify/require"
)

type scriptModel struct {
	script func(context.Context, domain.ModelSessionConfig, domain.UserInput) error
}

func (m scriptModel) Open(_ context.Context, config domain.ModelSessionConfig) (domain.ModelSession, error) {
	return &scriptSession{config: config, script: m.script, events: make(chan domain.ModelEvent, 2)}, nil
}

type scriptSession struct {
	config domain.ModelSessionConfig
	script func(context.Context, domain.ModelSessionConfig, domain.UserInput) error
	events chan domain.ModelEvent
}

func (s *scriptSession) Send(ctx context.Context, input domain.UserInput) error {
	if err := s.script(ctx, s.config, input); err != nil {
		return err
	}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: input.ID}
	s.events <- domain.ModelEvent{Kind: domain.ModelEventTurnFinished}
	return nil
}
func (s *scriptSession) Events() <-chan domain.ModelEvent { return s.events }
func (*scriptSession) ResumeToken() string                { return "token" }
func (*scriptSession) Idle() bool                         { return true }
func (*scriptSession) Close() error                       { return nil }

type testHub struct{ domain.ToolHub }

func (testHub) OpenSession(domain.SessionKey) (domain.MCPEndpoint, error) {
	return domain.MCPEndpoint{}, nil
}
func (testHub) CloseSession(domain.SessionKey) {}

func TestConsolidationIsAnOrdinaryTask(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fails], func(t *testing.T) {
			db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
			require.NoError(t, err)
			defer db.Close()
			store, err := memory.New(t.Context(), memory.Config{Path: t.TempDir(), Defaults: Procedures})
			require.NoError(t, err)
			episodes := repository.NewEpisodeRepository(db)
			events := repository.NewEventRepository(db)
			require.NoError(t, events.SaveInbound(t.Context(), domain.InboundEvent{Sender: domain.Sender{Kind: domain.SenderHuman}, Conversation: "room", Kind: domain.InboundEventMessage, Text: "The release is Friday", ReceivedAt: time.Now().Add(-time.Minute)}))
			inbox := repository.NewInboxRepository(db)
			item := reconcile.TaskItem("consolidate", "run procedure", "")
			item.Payload.Procedure = "consolidate"
			_, err = inbox.Enqueue(t.Context(), item)
			require.NoError(t, err)
			seen := make(chan string, 1)
			model := scriptModel{script: func(ctx context.Context, _ domain.ModelSessionConfig, input domain.UserInput) error {
				seen <- input.Text
				if fails {
					return errors.New("model failed")
				}
				raw, err := EpisodesTool(episodes, nil).Handler(ctx, json.RawMessage(`{}`))
				if err != nil {
					return err
				}
				var batch Episodes
				if err := json.Unmarshal(raw, &batch); err != nil {
					return err
				}
				if len(batch.Items) != 1 || batch.Items[0].Input != "The release is Friday" {
					return errors.New("missing episode")
				}
				return os.WriteFile(filepath.Join(store.Dir(), "topics/release.md"), []byte("The release is Friday"), 0600)
			}}
			r := reconcile.New(reconcile.Config{MaxAttempts: 1, Completed: Checkpoint(episodes), Procedure: func(ctx context.Context, name string) (string, error) {
				return store.Read(ctx, "procedures/"+name+".md")
			}}, inbox, repository.NewScheduleRepository(db), repository.NewSessionRepository(db), events, model, testHub{})
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- r.Start(ctx) }()
			select {
			case input := <-seen:
				require.Contains(t, input, "factual corrections")
			case <-time.After(time.Second):
				t.Fatal("no task input")
			}
			expected := "done"
			if fails {
				expected = "dead"
			}
			require.Eventually(t, func() bool {
				var status string
				return db.QueryRow("SELECT status FROM inbox WHERE source_key=?", item.SourceKey).Scan(&status) == nil && status == expected
			}, 10*time.Second, 5*time.Millisecond)
			if !fails {
				require.Eventually(t, func() bool { at, err := episodes.LastRun(t.Context()); return err == nil && at.After(item.ReadyAt) }, 10*time.Second, 5*time.Millisecond)
			}
			cancel()
			require.NoError(t, <-done)
			at, err := episodes.LastRun(t.Context())
			require.NoError(t, err)
			if fails {
				require.True(t, at.IsZero())
			} else {
				require.True(t, at.After(item.ReadyAt))
				text, err := store.Read(t.Context(), "topics/release.md")
				require.NoError(t, err)
				require.Equal(t, "The release is Friday", text)
			}
		})
	}
}

func TestSkippedProcedureAndContinuationKeepCheckpoint(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	defer db.Close()
	inbox := repository.NewInboxRepository(db)
	episodes := repository.NewEpisodeRepository(db)
	for i, text := range []string{"procedure", "watch exited with code 0\nfinished", "wake note"} {
		item := reconcile.TaskItem("consolidate", text, "")
		if i == 0 {
			item.Payload.Procedure = "consolidate"
		}
		_, err := inbox.Enqueue(t.Context(), item)
		require.NoError(t, err)
	}
	inputs := make(chan string, 3)
	model := scriptModel{script: func(_ context.Context, _ domain.ModelSessionConfig, input domain.UserInput) error {
		inputs <- input.Text
		return nil
	}}
	r := reconcile.New(reconcile.Config{Completed: Checkpoint(episodes), Procedure: func(context.Context, string) (string, error) { return "", nil }}, inbox, repository.NewScheduleRepository(db), repository.NewSessionRepository(db), repository.NewEventRepository(db), model, testHub{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	require.Eventually(t, func() bool { items, err := inbox.Active(t.Context()); return err == nil && len(items) == 0 }, 10*time.Second, 5*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	require.Len(t, inputs, 2)
	require.Equal(t, "watch exited with code 0\nfinished", <-inputs)
	require.Equal(t, "wake note", <-inputs)
	at, err := episodes.LastRun(t.Context())
	require.NoError(t, err)
	require.True(t, at.IsZero())
}
