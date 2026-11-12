package repository_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "test.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func TestSchemaReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test ?#%.db")
	for i := range 2 {
		db, err := repository.New(t.Context(), repository.Config{Path: path})
		require.NoError(t, err)
		if i == 0 {
			_, err = db.ExecContext(t.Context(), "INSERT INTO sessions(key, resume_token) VALUES ('session', 'resume')")
			require.NoError(t, err)
		} else {
			var token string
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT resume_token FROM sessions WHERE key='session'").Scan(&token))
			assert.Equal(t, "resume", token)
		}
		require.NoError(t, db.Close())
	}
}

func TestOpenErrors(t *testing.T) {
	db, err := repository.New(t.Context(), repository.Config{})
	require.Error(t, err)
	assert.Nil(t, db)
	path := filepath.Join(t.TempDir(), "corrupt.db")
	require.NoError(t, os.WriteFile(path, []byte("not a SQLite database"), 0o600))
	db, err = repository.New(t.Context(), repository.Config{Path: path})
	require.Error(t, err)
	assert.Nil(t, db)
}

func TestDatabasePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.db")
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	db, err := repository.New(t.Context(), repository.Config{Path: path})
	require.NoError(t, err)
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(t.Context(), "INSERT INTO oauth_tokens(server, access_token, token_type, refresh_token, expiry) VALUES ('server', 'access', 'Bearer', 'refresh', '2026-01-01T00:00:00.000000000Z')")
	require.NoError(t, err)
	for _, name := range []string{path, path + "-journal"} {
		info, err := os.Stat(name)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), name)
	}
}

func TestFTSIndex(t *testing.T) {
	db := openDB(t)
	for _, tt := range []struct {
		statement string
		count     int
	}{
		{`INSERT INTO events(id,kind,conversation,parent,sender_id,sender_display_name,sender_kind,text,attachments,mentions_me,received_at) VALUES (1, 'message', 'room', '', 'user', '', 'human', 'needle', '[]', 0, '2026-01-01T00:00:00.000000000Z')`, 1},
		{`UPDATE events SET text='other' WHERE id=1`, 0},
		{`UPDATE events SET text='needle' WHERE id=1`, 1},
		{`DELETE FROM events WHERE id=1`, 0},
	} {
		_, err := db.ExecContext(t.Context(), tt.statement)
		require.NoError(t, err)
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM events_fts WHERE events_fts MATCH '"needle"'`).Scan(&count))
		assert.Equal(t, tt.count, count)
	}
}
