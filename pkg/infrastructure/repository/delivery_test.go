package repository_test

import (
	"database/sql"
	"strings"
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

func TestRestartContinuesDeliveredTurnsOnce(t *testing.T) {
	db := openDB(t)
	inbox := repository.NewInboxRepository(db)
	sessions := repository.NewSessionRepository(db)
	for _, lane := range []string{"conv", "task"} {
		info := domain.SessionContext{Conversation: domain.ConversationRef(lane)}
		if lane == "task" {
			info.TaskName = "daily"
			info.Conversation = ""
		}
		item := domain.InboxItem{SourceKey: lane, SessionKey: domain.SessionKey(lane), Lane: lane, ReadyAt: time.Now(), Payload: domain.InboxPayload{Target: "thread", Context: info, Input: domain.UserInput{Text: "never replay"}}}
		deliveredTurn(t, db, item)
	}
	require.NoError(t, sessions.Recover(t.Context()))
	require.NoError(t, sessions.Recover(t.Context()))
	active, err := inbox.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, active, 2)
	for _, item := range active {
		require.Equal(t, string(item.SessionKey), item.Lane)
		require.Contains(t, []string{"conv", "task"}, item.Lane)
		require.Equal(t, 1, item.Payload.Continuations)
		require.Nil(t, item.Payload.Event)
		if item.Lane == "conv" {
			require.True(t, strings.HasPrefix(item.Payload.Input.Text, "previous turn was interrupted: restart\n"))
			require.Equal(t, domain.ConversationRef("thread"), item.Payload.Target)
			require.Equal(t, domain.SessionContext{Conversation: "conv"}, item.Payload.Context)
		} else {
			require.Equal(t, "previous turn was interrupted: restart", item.Payload.Input.Text)
			require.Empty(t, item.Payload.Target)
			require.Equal(t, domain.SessionContext{TaskName: "daily"}, item.Payload.Context)
		}
		require.Empty(t, item.Payload.MessageRef)
	}
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
		records, err := repository.NewEventRepository(db).ListModel(t.Context(), key, 10)
		require.NoError(t, err)
		require.Len(t, records, 1)
		require.Equal(t, domain.ModelEventError, records[0].Event.Kind)
		require.Equal(t, "previous turn was interrupted: restart", records[0].Event.Text)
		require.EqualError(t, records[0].Event.Err, "previous turn was interrupted: restart")
	}
	for _, continued := range active {
		_, err = inbox.Claim(t.Context(), continued.ID, time.Now())
		require.NoError(t, err)
		require.NoError(t, inbox.Delivered(t.Context(), continued.ID, domain.Session{Key: continued.SessionKey, Context: &continued.Payload.Context, Turn: &domain.SessionTurn{InputID: continued.ID, Continuations: continued.Payload.Continuations, Deadline: time.Now().Add(time.Hour)}}))
	}
	require.NoError(t, sessions.Recover(t.Context()))
	active, err = inbox.Active(t.Context())
	require.NoError(t, err)
	require.Empty(t, active)
}

func TestInterruptedConversationRefs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		payload   domain.InboxPayload
		unstarted bool
		want      domain.ConversationRef
		message   domain.ConversationRef
	}{
		{name: "top level", payload: domain.InboxPayload{Target: "ignored", MessageRef: "ignored", Event: &domain.InboundEvent{Conversation: "room", MessageRef: "message"}}, want: "room", message: "message"},
		{name: "continuation", payload: domain.InboxPayload{Target: "room", MessageRef: "message"}, want: "room", message: "message"},
		{name: "empty target", want: "room"},
		{name: "unstarted opening row", unstarted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openDB(t)
			inbox := repository.NewInboxRepository(db)
			sessions := repository.NewSessionRepository(db)
			info := domain.SessionContext{Conversation: "room", Workdir: "/work"}
			turn := &domain.SessionTurn{Deadline: time.Now().Add(time.Hour)}
			session := domain.Session{Key: "room", Context: &info, Turn: turn}
			id, err := inbox.Enqueue(t.Context(), domain.InboxItem{SourceKey: "opening", SessionKey: session.Key, Lane: "conv", ReadyAt: time.Now(), Payload: tc.payload})
			require.NoError(t, err)
			turn.InputID = id
			claimed, err := inbox.Claim(t.Context(), id, turn.Deadline)
			require.NoError(t, err)
			require.True(t, claimed)
			if !tc.unstarted {
				require.NoError(t, inbox.Delivered(t.Context(), id, session))
			}
			require.NoError(t, sessions.Save(t.Context(), session))
			require.NoError(t, sessions.Interrupt(t.Context(), session.Key, "model process exited"))
			active, err := inbox.Active(t.Context())
			require.NoError(t, err)
			if tc.want == "" {
				require.Len(t, active, 1)
				require.Equal(t, "opening", active[0].SourceKey)
				return
			}
			require.Len(t, active, 1)
			require.Equal(t, tc.want, active[0].Payload.Target)
			require.Equal(t, tc.message, active[0].Payload.MessageRef)
			require.Equal(t, info, active[0].Payload.Context)
			require.Nil(t, active[0].Payload.Event)
			require.True(t, strings.HasPrefix(active[0].Payload.Input.Text, "previous turn was interrupted: model process exited\n"))
		})
	}
}

func TestRecoverContinuesConversationWithMalformedOpeningPayload(t *testing.T) {
	db := openDB(t)
	inbox := repository.NewInboxRepository(db)
	sessions := repository.NewSessionRepository(db)
	info := domain.SessionContext{Conversation: "room"}
	id := deliveredTurn(t, db, domain.InboxItem{SourceKey: "opening", SessionKey: "room", Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Context: info, Target: "thread"}})
	malformed := `{"Target":"wrong-thread","MessageRef":"wrong-message","Continuations":"invalid"}`
	_, err := db.ExecContext(t.Context(), `UPDATE inbox SET payload=? WHERE id=?`, malformed, id)
	require.NoError(t, err)
	require.NoError(t, sessions.Recover(t.Context()))
	require.NoError(t, sessions.Recover(t.Context()))
	active, err := inbox.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.NotEqual(t, id, active[0].ID)
	require.Equal(t, "conv", active[0].Lane)
	require.Equal(t, domain.ConversationRef("room"), active[0].Payload.Target)
	require.Empty(t, active[0].Payload.MessageRef)
	require.Equal(t, info, active[0].Payload.Context)
	require.Equal(t, 1, active[0].Payload.Continuations)
	require.Nil(t, active[0].Payload.Event)
	saved, err := sessions.Get(t.Context(), "room")
	require.NoError(t, err)
	require.Nil(t, saved.Turn)
	var status, payload string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT status,payload FROM inbox WHERE id=?`, id).Scan(&status, &payload))
	require.Equal(t, "done", status)
	require.Equal(t, malformed, payload)
}

func TestInterruptStopReasons(t *testing.T) {
	for _, reason := range []string{domain.TurnDeadlineExceeded, domain.DeliveryDeadlineExceeded, domain.ConversationNoLongerAccepted} {
		for _, lane := range []string{"conv", "task"} {
			t.Run(reason+"/"+lane, func(t *testing.T) {
				db := openDB(t)
				inbox := repository.NewInboxRepository(db)
				sessions := repository.NewSessionRepository(db)
				info := domain.SessionContext{Conversation: "room"}
				if lane == "task" {
					info.TaskName = "daily"
				}
				item := domain.InboxItem{SourceKey: "opening", SessionKey: "session", Lane: lane, ReadyAt: time.Now(), Payload: domain.InboxPayload{Context: info}}
				deliveredTurn(t, db, item)
				require.NoError(t, sessions.Interrupt(t.Context(), item.SessionKey, reason))
				active, err := inbox.Active(t.Context())
				require.NoError(t, err)
				if lane == "conv" {
					require.Empty(t, active)
				} else {
					require.Len(t, active, 1)
					if reason == domain.ConversationNoLongerAccepted {
						require.Equal(t, "task", active[0].Lane)
						require.Equal(t, 1, active[0].Payload.Continuations)
						require.Equal(t, "previous turn was interrupted: "+reason, active[0].Payload.Input.Text)
					} else {
						require.Equal(t, "Task daily stopped: "+reason, active[0].Payload.Notice)
					}
				}
				saved, err := sessions.Get(t.Context(), item.SessionKey)
				require.NoError(t, err)
				require.Nil(t, saved.Turn)
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
			item := domain.InboxItem{SourceKey: "unsent", SessionKey: "session", Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: "original"}}}
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
			item := domain.InboxItem{SourceKey: "opening", SessionKey: "task", Lane: "task", ReadyAt: time.Now(), Payload: domain.InboxPayload{Context: domain.SessionContext{TaskName: "daily"}, Input: domain.UserInput{Text: "original"}}}
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
				require.Equal(t, "previous turn was interrupted: restart", followup.Payload.Input.Text)
				require.Equal(t, 1, followup.Payload.Continuations)
			}
		})
	}
}
