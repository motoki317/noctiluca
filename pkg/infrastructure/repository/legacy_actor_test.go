package repository_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func TestLegacyActorsRemainReadable(t *testing.T) {
	actor := "member"
	config := repository.Config{Path: filepath.Join(t.TempDir(), "legacy.db")}
	db, err := repository.New(t.Context(), config)
	require.NoError(t, err)
	at := time.Now().UTC()
	info := fmt.Sprintf(`{"Actor":%q,"Conversation":"room","Direct":true,"Workdir":"/work"}`, actor)
	payload := fmt.Sprintf(`{"Context":%s,"Input":{"Text":"queued"}}`, info)
	_, err = db.Exec(`INSERT INTO sessions(key,resume_token,context_json) VALUES('room','resume',?)`, info)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO inbox(source_key,session_key,actor,payload,ready_at,lane,status,retry_at,deadline) VALUES('legacy','room',?, ?,?,'conv','pending',?,?)`, actor, payload, at, at, at)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO schedules(name,cron,tz,prompt,workdir,actor,next_run) VALUES('daily','0 9 * * *','UTC','check','/work',?,?)`, actor, at)
	require.NoError(t, err)
	watch := fmt.Sprintf(`{"Actor":%q,"SessionKey":"room","Lane":"conv","Payload":%s}`, actor, payload)
	_, err = db.Exec(`INSERT INTO watches(command,cwd,item) VALUES('true','/work',?)`, watch)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = repository.New(t.Context(), config)
	require.NoError(t, err)
	defer db.Close()
	wantInfo := domain.SessionContext{Conversation: "room", Direct: true, Workdir: "/work"}
	session, err := repository.NewSessionRepository(db).Get(t.Context(), "room")
	require.NoError(t, err)
	require.Equal(t, "resume", session.ResumeToken)
	require.Equal(t, &wantInfo, session.Context)
	inbox := repository.NewInboxRepository(db)
	items, err := inbox.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, wantInfo, items[0].Payload.Context)
	require.Equal(t, "queued", items[0].Payload.Input.Text)
	items[0].SourceKey = "new"
	_, err = inbox.Enqueue(t.Context(), items[0])
	require.NoError(t, err)
	schedules := repository.NewScheduleRepository(db)
	jobs, err := schedules.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, []domain.Schedule{{Name: "daily", Cron: "0 9 * * *", TZ: "UTC", Prompt: "check", Workdir: "/work", NextRun: at}}, jobs)
	jobs[0].Prompt = "updated"
	require.NoError(t, schedules.Save(t.Context(), jobs[0]))
	jobs[0].Name = "new"
	require.NoError(t, schedules.Save(t.Context(), jobs[0]))
	watches := repository.NewWatchRepository(db)
	pending, err := watches.Unfinished(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, wantInfo, pending[0].Item.Payload.Context)
	require.NoError(t, watches.Complete(t.Context(), pending[0].ID, "finished", at))
	items, err = inbox.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 3)
	require.Equal(t, "finished", items[2].Payload.Input.Text)
	require.Equal(t, wantInfo, items[2].Payload.Context)
}
