package repository_test

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func TestStatePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := repository.New(t.Context(), repository.Config{Path: path})
	require.NoError(t, err)
	sessions := repository.NewSessionRepository(db)
	_, err = sessions.Get(t.Context(), "missing")
	require.ErrorIs(t, err, domain.ErrNotFound)
	require.NoError(t, sessions.Save(t.Context(), domain.Session{Key: "session", ResumeToken: "old"}))
	session, err := sessions.Get(t.Context(), "session")
	require.NoError(t, err)
	assert.Nil(t, session.Context)
	savedSession := domain.Session{Key: "session", ResumeToken: "new", Context: &domain.SessionContext{Conversation: "thread", Direct: true, Workdir: "/work"}}
	require.NoError(t, sessions.Save(t.Context(), savedSession))

	schedules := repository.NewScheduleRepository(db)
	schedule := domain.Schedule{Name: "daily", Cron: "0 9 * * *", TZ: "Asia/Tokyo", Prompt: "preview", Workdir: "/workspace"}
	require.NoError(t, schedules.Save(t.Context(), schedule))
	schedule.Prompt = "post"
	require.NoError(t, schedules.Save(t.Context(), schedule))

	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tokens := repository.NewOAuthTokenRepository(db)
	_, err = tokens.Get(t.Context(), "missing")
	require.ErrorIs(t, err, domain.ErrNotFound)
	token := domain.OAuthToken{Registration: json.RawMessage(`{"client_id":"registered"}`), AccessToken: "access", TokenType: "Bearer", RefreshToken: "refresh", Expiry: at}
	require.NoError(t, tokens.Save(t.Context(), "server", token))
	token.AccessToken = "renewed"
	token.Expiry = time.Time{}
	require.NoError(t, tokens.Save(t.Context(), "server", token))
	require.NoError(t, db.Close())

	db, err = repository.New(t.Context(), repository.Config{Path: path})
	require.NoError(t, err)
	defer db.Close()
	session, err = repository.NewSessionRepository(db).Get(t.Context(), "session")
	require.NoError(t, err)
	assert.Equal(t, savedSession, session)
	schedules = repository.NewScheduleRepository(db)
	storedSchedules, err := schedules.List(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []domain.Schedule{schedule}, storedSchedules)
	require.NoError(t, schedules.Delete(t.Context(), "daily"))
	require.NoError(t, schedules.Delete(t.Context(), "daily"))
	storedSchedules, err = schedules.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, storedSchedules)
	storedToken, err := repository.NewOAuthTokenRepository(db).Get(t.Context(), "server")
	require.NoError(t, err)
	assert.Equal(t, token, storedToken)
}

func TestSchemaFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incompatible.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `CREATE TABLE oauth_tokens (server TEXT PRIMARY KEY, access_token TEXT, token_type TEXT, refresh_token TEXT, expiry DATETIME);
		INSERT INTO oauth_tokens VALUES ('saved', 'private-token', 'Bearer', '', '2036-01-01')`)
	require.NoError(t, err)
	failed, err := repository.New(t.Context(), repository.Config{Path: path})
	require.ErrorContains(t, err, "database schema mismatch")
	assert.Nil(t, failed)
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_schema WHERE name='events'`).Scan(&count))
	assert.Zero(t, count)
	var token string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT access_token FROM oauth_tokens WHERE server='saved'`).Scan(&token))
	assert.Equal(t, "private-token", token)
}
