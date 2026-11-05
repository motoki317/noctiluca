package repository_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func TestRestartContinuesDeliveredTaskOnce(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	defer db.Close()
	inbox := repository.NewInboxRepository(db)
	sessions := repository.NewSessionRepository(db)
	for _, lane := range []string{"conv", "task"} {
		info := domain.SessionContext{Actor: domain.ActorOwner, Conversation: domain.ConversationRef(lane)}
		if lane == "task" {
			info.TaskName = "daily"
			info.Conversation = ""
		}
		item := domain.InboxItem{SourceKey: lane, SessionKey: domain.SessionKey(lane), Actor: domain.ActorOwner, Lane: lane, ReadyAt: time.Now(), Payload: domain.InboxPayload{Context: info, Input: domain.UserInput{Text: "never replay"}}}
		id, err := inbox.Enqueue(t.Context(), item)
		require.NoError(t, err)
		_, err = inbox.Claim(t.Context(), id, time.Now())
		require.NoError(t, err)
		require.NoError(t, inbox.Delivered(t.Context(), id, domain.Session{Key: item.SessionKey, ResumeToken: "resume", Context: &info, Turn: &domain.SessionTurn{Deadline: time.Now().Add(time.Hour)}}))
	}
	require.NoError(t, sessions.Recover(t.Context()))
	require.NoError(t, sessions.Recover(t.Context()))
	active, err := inbox.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "task", active[0].Lane)
	require.Equal(t, "previous turn was interrupted: restart", active[0].Payload.Input.Text)
	require.Equal(t, 1, active[0].Payload.Continuations)
	var done, events int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM inbox WHERE status='done'").Scan(&done))
	require.Equal(t, 2, done)
	require.NoError(t, db.QueryRow("SELECT count(*) FROM model_events").Scan(&events))
	require.Equal(t, 2, events)
	for _, key := range []domain.SessionKey{"conv", "task"} {
		s, err := sessions.Get(t.Context(), key)
		require.NoError(t, err)
		require.Nil(t, s.Turn)
		require.Equal(t, "resume", s.ResumeToken)
	}
	continued := active[0]
	_, err = inbox.Claim(t.Context(), continued.ID, time.Now())
	require.NoError(t, err)
	require.NoError(t, inbox.Delivered(t.Context(), continued.ID, domain.Session{Key: continued.SessionKey, Context: &continued.Payload.Context, Turn: &domain.SessionTurn{Continuations: continued.Payload.Continuations, Deadline: time.Now().Add(time.Hour)}}))
	require.NoError(t, sessions.Recover(t.Context()))
	active, err = inbox.Active(t.Context())
	require.NoError(t, err)
	require.Empty(t, active)
}

func TestUnsentHandoffReturnsPending(t *testing.T) {
	for _, reason := range []string{"restart", "delivery deadline exceeded"} {
		for _, kind := range []string{"conv", "task", "notice"} {
			t.Run(reason+"/"+kind, func(t *testing.T) {
				db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
				require.NoError(t, err)
				defer db.Close()
				inbox := repository.NewInboxRepository(db)
				sessions := repository.NewSessionRepository(db)
				item := domain.InboxItem{SourceKey: "unsent", SessionKey: "session", Actor: domain.ActorOwner, Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: "original"}}}
				switch kind {
				case "task":
					item.Lane = "task"
					item.Payload.Context.TaskName = "daily"
				case "notice":
					item.Payload.Notice = "owner note"
				}
				id, err := inbox.Enqueue(t.Context(), item)
				require.NoError(t, err)
				_, err = inbox.Claim(t.Context(), id, time.Now())
				require.NoError(t, err)
				if reason == "restart" {
					require.NoError(t, sessions.Recover(t.Context()))
				} else {
					require.NoError(t, sessions.Interrupt(t.Context(), item.SessionKey, reason))
				}
				require.NoError(t, sessions.Recover(t.Context()))
				active, err := inbox.Active(t.Context())
				require.NoError(t, err)
				require.Len(t, active, 1)
				require.Equal(t, id, active[0].ID)
				require.Equal(t, "pending", active[0].Status)
				require.Equal(t, item.Payload, active[0].Payload)
				var count int
				require.NoError(t, db.QueryRow("SELECT count(*) FROM model_events").Scan(&count))
				require.Zero(t, count)
			})
		}
	}
}
