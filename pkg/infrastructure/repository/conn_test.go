package repository_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/migrations"
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
		for _, table := range []string{"events", "sends", "sessions", "schedules", "inbox", "watches", "oauth_tokens"} {
			var count int
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?", table).Scan(&count))
			assert.Equal(t, 1, count, table)
		}
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_schema WHERE name='kv'").Scan(&count))
		assert.Zero(t, count)
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
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestOpenErrors(t *testing.T) {
	for _, path := range []string{"", t.TempDir(), filepath.Join(t.TempDir(), "missing", "test.db")} {
		db, err := repository.New(t.Context(), repository.Config{Path: path})
		require.Error(t, err)
		assert.Nil(t, db)
	}
	path := filepath.Join(t.TempDir(), "corrupt.db")
	require.NoError(t, os.WriteFile(path, []byte("not a SQLite database"), 0o600))
	db, err := repository.New(t.Context(), repository.Config{Path: path})
	require.Error(t, err)
	assert.Nil(t, db)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	db, err = repository.New(ctx, repository.Config{Path: filepath.Join(t.TempDir(), "cancelled.db")})
	require.ErrorIs(t, err, context.Canceled)
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

func TestRejectSchemaDriftBeforeApplyingSchema(t *testing.T) {
	for _, column := range []string{"message_ref", "turn_json", "registration_json"} {
		t.Run(column, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.db")
			db, err := driver.Open(path, fts5.Register)
			require.NoError(t, err)
			lines := strings.Split(migrations.Schema, "\n")
			for i, line := range lines {
				if strings.HasPrefix(strings.TrimSpace(line), column+" ") {
					lines[i] = ""
				}
			}
			schema := strings.ReplaceAll(strings.Join(lines, "\n"), ",\n\n);", "\n);")
			_, err = db.Exec(schema)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO oauth_tokens(server,access_token,token_type,refresh_token,expiry) VALUES('saved','private-token','Bearer','','2036-01-01')`)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			opened, err := repository.New(t.Context(), repository.Config{Path: path})
			require.Nil(t, opened)
			require.ErrorContains(t, err, "database schema mismatch")
			require.ErrorContains(t, err, "recreate the database")
			require.ErrorContains(t, err, "oauth_tokens")
			db, err = driver.Open(path, fts5.Register)
			require.NoError(t, err)
			defer db.Close()
			var token string
			require.NoError(t, db.QueryRow(`SELECT access_token FROM oauth_tokens WHERE server='saved'`).Scan(&token))
			require.Equal(t, "private-token", token)
		})
	}
}
