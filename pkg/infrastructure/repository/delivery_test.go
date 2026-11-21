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
	claimed, err := inbox.Claim(t.Context(), id)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, inbox.Delivered(t.Context(), id, domain.Session{Key: item.SessionKey, ResumeToken: "resume", Context: &item.Payload.Context,
		Turn: &domain.SessionTurn{InputID: id}}))
	return id
}

func TestRestartContinuesDeliveredTurnOnce(t *testing.T) {
	db := openDB(t)
	inbox := repository.NewInboxRepository(db)
	sessions := repository.NewSessionRepository(db)
	item := domain.InboxItem{SourceKey: "opening", SessionKey: domain.MainSessionKey, Lane: domain.LaneConversation, ReadyAt: time.Now()}
	deliveredTurn(t, db, item)
	// An expired deadline in legacy turn JSON must not suppress recovery.
	_, err := db.Exec(`UPDATE sessions SET turn_json=json_set(turn_json,'$.Deadline','2000-01-01T00:00:00Z') WHERE key=?`, domain.MainSessionKey)
	require.NoError(t, err)
	require.NoError(t, sessions.Recover(t.Context()))
	require.NoError(t, sessions.Recover(t.Context()))
	active, err := inbox.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, active, 1)
	continued := active[0]
	require.Equal(t, domain.MainSessionKey, continued.SessionKey)
	require.Equal(t, domain.LaneConversation, continued.Lane)
	require.Equal(t, 1, continued.Payload.Continuations)
	require.Nil(t, continued.Payload.Event)
	require.Contains(t, continued.Payload.Input.Text, "Finish each unfinished part")
	_, err = inbox.Claim(t.Context(), continued.ID)
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

func TestInterruptContinuesRegardlessOfTaskOrReason(t *testing.T) {
	for _, reason := range []string{"model process exited", "shutdown"} {
		for _, lane := range []domain.Lane{domain.LaneConversation, domain.LaneTask} {
			t.Run(reason+"/"+string(lane), func(t *testing.T) {
				db := openDB(t)
				sessions := repository.NewSessionRepository(db)
				item := domain.InboxItem{SourceKey: "opening", SessionKey: domain.MainSessionKey, Lane: lane, ReadyAt: time.Now(), Payload: domain.InboxPayload{Context: domain.SessionContext{TaskName: "daily"}}}
				deliveredTurn(t, db, item)
				require.NoError(t, sessions.Interrupt(t.Context(), domain.MainSessionKey, reason))
				active, err := repository.NewInboxRepository(db).Active(t.Context())
				require.NoError(t, err)
				require.Len(t, active, 1)
				require.Equal(t, domain.LaneConversation, active[0].Lane)
				require.Equal(t, 1, active[0].Payload.Continuations)
				require.Contains(t, active[0].Payload.Input.Text, "previous turn was interrupted: "+reason)
				require.Contains(t, active[0].Payload.Input.Text, "Finish each unfinished part")
				require.Empty(t, active[0].Payload.Notice)
			})
		}
	}
}

func TestUnsentHandoffReturnsPending(t *testing.T) {
	for _, reason := range []string{"restart", "model process exited"} {
		t.Run(reason, func(t *testing.T) {
			db := openDB(t)
			inbox := repository.NewInboxRepository(db)
			sessions := repository.NewSessionRepository(db)
			item := domain.InboxItem{SourceKey: "unsent", SessionKey: domain.MainSessionKey, Lane: domain.LaneConversation, ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: "original"}}}
			id, err := inbox.Enqueue(t.Context(), item)
			require.NoError(t, err)
			_, err = inbox.Claim(t.Context(), id)
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
			require.Equal(t, domain.InboxPending, active[0].Status)
			require.Zero(t, active[0].Attempts)
			require.Equal(t, item.Payload, active[0].Payload)
			var count int
			require.NoError(t, db.QueryRow("SELECT count(*) FROM model_events").Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestInterruptChecksOpeningInput(t *testing.T) {
	const noOpeningInput = "no opening input"
	for _, status := range []domain.InboxStatus{domain.InboxDispatched, domain.InboxDead, domain.InboxDone, noOpeningInput} {
		t.Run(string(status), func(t *testing.T) {
			db := openDB(t)
			inbox := repository.NewInboxRepository(db)
			sessions := repository.NewSessionRepository(db)
			item := domain.InboxItem{SourceKey: "opening", SessionKey: domain.MainSessionKey, Lane: domain.LaneTask, ReadyAt: time.Now(), Payload: domain.InboxPayload{Context: domain.SessionContext{TaskName: "daily"}, Input: domain.UserInput{Text: "original"}}}
			id, err := inbox.Enqueue(t.Context(), item)
			require.NoError(t, err)
			claimed, err := inbox.Claim(t.Context(), id)
			require.NoError(t, err)
			require.True(t, claimed)
			turn := &domain.SessionTurn{InputID: id}
			if status == noOpeningInput {
				turn.InputID = 0
			}
			state := domain.Session{Key: item.SessionKey, ResumeToken: "resume", Context: &item.Payload.Context, Turn: turn}
			require.NoError(t, sessions.Save(t.Context(), state))
			expectedStatus := domain.InboxPending
			switch status {
			case domain.InboxDead:
				_, err := inbox.Fail(t.Context(), id, "refused", 1, time.Now().Add(time.Hour))
				require.NoError(t, err)
				expectedStatus = domain.InboxDead
			case domain.InboxDone:
				require.NoError(t, inbox.Delivered(t.Context(), id, state))
				expectedStatus = domain.InboxDone
				// Later unstarted input must not hide an acknowledged opening item.
				item.SourceKey = "queued"
				queued, err := inbox.Enqueue(t.Context(), item)
				require.NoError(t, err)
				_, err = inbox.Claim(t.Context(), queued)
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
			var originalStatus domain.InboxStatus
			require.NoError(t, db.QueryRow("SELECT status FROM inbox WHERE id=?", id).Scan(&originalStatus))
			require.Equal(t, expectedStatus, originalStatus)
			var notes, followups int
			require.NoError(t, db.QueryRow("SELECT count(*) FROM model_events WHERE kind='error'").Scan(&notes))
			require.NoError(t, db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'interrupted:%'").Scan(&followups))
			expected := 0
			if status == domain.InboxDone || status == noOpeningInput {
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
		item := domain.InboxItem{SessionKey: key, SourceKey: string(key), Lane: domain.LaneConversation}
		id, err := inbox.Enqueue(t.Context(), item)
		require.NoError(t, err)
		ids[key] = id
		claimed, err := inbox.Claim(t.Context(), id)
		require.NoError(t, err)
		require.True(t, claimed)
	}
	status := func(key domain.SessionKey, want domain.InboxStatus) {
		t.Helper()
		var got domain.InboxStatus
		require.NoError(t, db.QueryRow("SELECT status FROM inbox WHERE id=?", ids[key]).Scan(&got))
		require.Equal(t, want, got)
	}
	require.NoError(t, sessions.Interrupt(t.Context(), domain.MainSessionKey, "exit"))
	for _, key := range []domain.SessionKey{"old-room", "task:old", domain.MainSessionKey} {
		status(key, domain.InboxPending)
	}
	status("notice:owner", domain.InboxDispatched)
	_, err := inbox.Claim(t.Context(), ids["old-room"])
	require.NoError(t, err)
	require.NoError(t, sessions.Interrupt(t.Context(), "notice:owner", "exit"))
	status("old-room", domain.InboxDispatched)
	status("notice:owner", domain.InboxPending)
	_, err = inbox.Claim(t.Context(), ids["notice:owner"])
	require.NoError(t, err)
	require.NoError(t, sessions.Recover(t.Context()))
	status("old-room", domain.InboxPending)
	status("notice:owner", domain.InboxPending)
}
