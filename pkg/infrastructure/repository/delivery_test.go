package repository_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func deliveredTurn(t *testing.T, db *sql.DB, item domain.InboxItem) int64 {
	t.Helper()
	inbox := repository.NewInboxRepository(db)
	id, err := inbox.Enqueue(t.Context(), item)
	require.NoError(t, err)
	deadline := time.Now().Add(time.Hour)
	claimed, err := inbox.Claim(t.Context(), id, deadline)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, inbox.Delivered(t.Context(), id, domain.Session{Key: item.SessionKey, ResumeToken: "resume", Context: &item.Payload.Context,
		Turn: &domain.SessionTurn{InputID: id, Deadline: deadline}}))
	return id
}

func TestRestartContinuesDeliveredTurnOnce(t *testing.T) {
	db := openDB(t)
	inbox := repository.NewInboxRepository(db)
	sessions := repository.NewSessionRepository(db)
	item := domain.InboxItem{SourceKey: "opening", SessionKey: domain.MainSessionKey, Lane: "conv", ReadyAt: time.Now()}
	deliveredTurn(t, db, item)
	require.NoError(t, sessions.Recover(t.Context()))
	require.NoError(t, sessions.Recover(t.Context()))
	active, err := inbox.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, active, 1)
	continued := active[0]
	require.Equal(t, domain.MainSessionKey, continued.SessionKey)
	require.Equal(t, "conv", continued.Lane)
	require.Equal(t, 1, continued.Payload.Continuations)
	require.Nil(t, continued.Payload.Event)
	require.Contains(t, continued.Payload.Input.Text, "finish each unfinished part")
	_, err = inbox.Claim(t.Context(), continued.ID, time.Now())
	require.NoError(t, err)
	require.NoError(t, inbox.Delivered(t.Context(), continued.ID, domain.Session{Key: domain.MainSessionKey, ResumeToken: "resume", Turn: &domain.SessionTurn{InputID: continued.ID, Continuations: 1}}))
	require.NoError(t, sessions.Recover(t.Context()))
	active, err = inbox.Active(t.Context())
	require.NoError(t, err)
	require.Empty(t, active)
	saved, err := sessions.Get(t.Context(), domain.MainSessionKey)
	require.NoError(t, err)
	require.Nil(t, saved.Turn)
	require.Equal(t, "resume", saved.ResumeToken)
}

func TestInterruptContinuesRegardlessOfTaskOrDeadline(t *testing.T) {
	for _, reason := range []string{"model process exited", domain.TurnDeadlineExceeded, domain.DeliveryDeadlineExceeded} {
		for _, lane := range []string{"conv", "task"} {
			t.Run(reason+"/"+lane, func(t *testing.T) {
				db := openDB(t)
				sessions := repository.NewSessionRepository(db)
				item := domain.InboxItem{SourceKey: "opening", SessionKey: domain.MainSessionKey, Lane: lane, ReadyAt: time.Now(), Payload: domain.InboxPayload{Context: domain.SessionContext{TaskName: "daily"}}}
				deliveredTurn(t, db, item)
				require.NoError(t, sessions.Interrupt(t.Context(), domain.MainSessionKey, reason))
				active, err := repository.NewInboxRepository(db).Active(t.Context())
				require.NoError(t, err)
				require.Len(t, active, 1)
				require.Equal(t, "conv", active[0].Lane)
				require.Equal(t, 1, active[0].Payload.Continuations)
				require.Contains(t, active[0].Payload.Input.Text, "previous turn was interrupted: "+reason)
				require.Contains(t, active[0].Payload.Input.Text, "finish each unfinished part")
				require.Empty(t, active[0].Payload.Notice)
			})
		}
	}
}

func TestUnsentHandoffReturnsPending(t *testing.T) {
	for _, reason := range []string{"restart", domain.DeliveryDeadlineExceeded} {
		t.Run(reason, func(t *testing.T) {
			db := openDB(t)
			inbox := repository.NewInboxRepository(db)
			sessions := repository.NewSessionRepository(db)
			item := domain.InboxItem{SourceKey: "unsent", SessionKey: domain.MainSessionKey, Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: "original"}}}
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

func TestInterruptChecksOpeningInput(t *testing.T) {
	for _, status := range []string{"dispatched", "dead", "done", "legacy"} {
		t.Run(status, func(t *testing.T) {
			db := openDB(t)
			inbox := repository.NewInboxRepository(db)
			sessions := repository.NewSessionRepository(db)
			item := domain.InboxItem{SourceKey: "opening", SessionKey: domain.MainSessionKey, Lane: "task", ReadyAt: time.Now(), Payload: domain.InboxPayload{Context: domain.SessionContext{TaskName: "daily"}, Input: domain.UserInput{Text: "original"}}}
			id, err := inbox.Enqueue(t.Context(), item)
			require.NoError(t, err)
			claimed, err := inbox.Claim(t.Context(), id, time.Now().Add(time.Hour))
			require.NoError(t, err)
			require.True(t, claimed)
			turn := &domain.SessionTurn{InputID: id, Deadline: time.Now().Add(time.Hour)}
			if status == "legacy" {
				turn.InputID = 0
			}
			state := domain.Session{Key: item.SessionKey, ResumeToken: "resume", Context: &item.Payload.Context, Turn: turn}
			require.NoError(t, sessions.Save(t.Context(), state))
			expectedStatus := "pending"
			switch status {
			case "dead":
				_, err := inbox.Finish(t.Context(), id, "refused", 1, time.Now().Add(time.Hour))
				require.NoError(t, err)
				expectedStatus = "dead"
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
			require.NoError(t, sessions.Recover(t.Context()))
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
				require.Contains(t, followup.Payload.Input.Text, "previous turn was interrupted: restart\n")
				require.Equal(t, 1, followup.Payload.Continuations)
			}
		})
	}
}

func TestInterruptResetsOldKeysAndKeepsNoticeIndependent(t *testing.T) {
	db := openDB(t)
	inbox := repository.NewInboxRepository(db)
	sessions := repository.NewSessionRepository(db)
	ids := make(map[domain.SessionKey]int64)
	for _, key := range []domain.SessionKey{"old-room", "task:old", domain.MainSessionKey, "notice:owner"} {
		item := domain.InboxItem{SessionKey: key, SourceKey: string(key), Lane: "conv"}
		id, err := inbox.Enqueue(t.Context(), item)
		require.NoError(t, err)
		ids[key] = id
		claimed, err := inbox.Claim(t.Context(), id, time.Now())
		require.NoError(t, err)
		require.True(t, claimed)
	}
	status := func(key domain.SessionKey, want string) {
		t.Helper()
		var got string
		require.NoError(t, db.QueryRow("SELECT status FROM inbox WHERE id=?", ids[key]).Scan(&got))
		require.Equal(t, want, got)
	}
	require.NoError(t, sessions.Interrupt(t.Context(), domain.MainSessionKey, "exit"))
	for _, key := range []domain.SessionKey{"old-room", "task:old", domain.MainSessionKey} {
		status(key, "pending")
	}
	status("notice:owner", "dispatched")
	_, err := inbox.Claim(t.Context(), ids["old-room"], time.Now())
	require.NoError(t, err)
	require.NoError(t, sessions.Interrupt(t.Context(), "notice:owner", "exit"))
	status("old-room", "dispatched")
	status("notice:owner", "pending")
	_, err = inbox.Claim(t.Context(), ids["notice:owner"], time.Now())
	require.NoError(t, err)
	require.NoError(t, sessions.Recover(t.Context()))
	status("old-room", "pending")
	status("notice:owner", "pending")
}
