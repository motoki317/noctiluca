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
		info := domain.SessionContext{Conversation: domain.ConversationRef(lane)}
		if lane == "task" {
			info.TaskName = "daily"
			info.Conversation = ""
		}
		item := domain.InboxItem{SourceKey: lane, SessionKey: domain.SessionKey(lane), Lane: lane, ReadyAt: time.Now(), Payload: domain.InboxPayload{Context: info, Input: domain.UserInput{Text: "never replay"}}}
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
				item := domain.InboxItem{SourceKey: "unsent", SessionKey: "session", Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: "original"}}}
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

func TestInterruptChecksOpeningInput(t *testing.T) {
	for _, reason := range []string{"restart", domain.DeliveryDeadlineExceeded} {
		for _, status := range []string{"dispatched", "pending", "dead", "done", "legacy", "missing"} {
			t.Run(reason+"/"+status, func(t *testing.T) {
				db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
				require.NoError(t, err)
				defer db.Close()
				inbox := repository.NewInboxRepository(db)
				sessions := repository.NewSessionRepository(db)
				item := domain.InboxItem{SourceKey: "opening", SessionKey: "task", Lane: "task", ReadyAt: time.Now(), Payload: domain.InboxPayload{Context: domain.SessionContext{TaskName: "daily"}, Input: domain.UserInput{Text: "original"}}}
				id, err := inbox.Enqueue(t.Context(), item)
				require.NoError(t, err)
				claimed, err := inbox.Claim(t.Context(), id, time.Now().Add(time.Hour))
				require.NoError(t, err)
				require.True(t, claimed)
				turn := &domain.SessionTurn{InputID: id, Deadline: time.Now().Add(time.Hour)}
				switch status {
				case "legacy":
					turn.InputID = 0
				case "missing":
					turn.InputID = id + 1
				}
				state := domain.Session{Key: item.SessionKey, ResumeToken: "resume", Context: &item.Payload.Context, Turn: turn}
				require.NoError(t, sessions.Save(t.Context(), state))
				expectedStatus := "pending"
				switch status {
				case "pending", "dead":
					limit := 3
					if status == "dead" {
						limit = 1
					}
					_, err := inbox.Finish(t.Context(), id, "refused", limit, time.Now().Add(time.Hour))
					require.NoError(t, err)
					expectedStatus = status
				case "done":
					require.NoError(t, inbox.Delivered(t.Context(), id, state))
					expectedStatus = "done"
					// Later unstarted input must not hide an acknowledged opening item.
					item.SourceKey = "queued"
					queued, err := inbox.Enqueue(t.Context(), item)
					require.NoError(t, err)
					_, err = inbox.Claim(t.Context(), queued, time.Now().Add(time.Hour))
					require.NoError(t, err)
				}
				events := repository.NewEventRepository(db)
				require.NoError(t, events.SaveModel(t.Context(), domain.ModelRecord{SessionKey: item.SessionKey, Event: domain.ModelEvent{Kind: domain.ModelEventTurnStarted}, RecordedAt: time.Now()}))
				before, err := events.InputContext(t.Context(), item.SessionKey, "", time.Now())
				require.NoError(t, err)
				require.True(t, before.Running)
				if reason == "restart" {
					require.NoError(t, sessions.Recover(t.Context()))
				} else {
					require.NoError(t, sessions.Interrupt(t.Context(), item.SessionKey, reason))
				}
				require.NoError(t, sessions.Recover(t.Context()))
				saved, err := sessions.Get(t.Context(), item.SessionKey)
				require.NoError(t, err)
				require.Nil(t, saved.Turn)
				require.Equal(t, "resume", saved.ResumeToken)
				after, err := events.InputContext(t.Context(), item.SessionKey, "", time.Now())
				require.NoError(t, err)
				require.False(t, after.Running)
				var originalStatus string
				require.NoError(t, db.QueryRow("SELECT status FROM inbox WHERE id=?", id).Scan(&originalStatus))
				require.Equal(t, expectedStatus, originalStatus)
				var notes, followups int
				require.NoError(t, db.QueryRow("SELECT count(*) FROM model_events WHERE kind='error'").Scan(&notes))
				require.NoError(t, db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'interrupted:%'").Scan(&followups))
				expected := 0
				if status == "done" || status == "legacy" {
					expected = 1
				}
				require.Equal(t, 1, notes)
				require.Equal(t, expected, followups)
				if expected != 0 {
					active, err := inbox.Active(t.Context())
					require.NoError(t, err)
					followup := active[len(active)-1]
					if reason == "restart" {
						require.Equal(t, "previous turn was interrupted: restart", followup.Payload.Input.Text)
						require.Equal(t, 1, followup.Payload.Continuations)
					} else {
						require.Equal(t, "Task daily stopped: delivery deadline exceeded", followup.Payload.Notice)
					}
				}
			})
		}
	}
}
