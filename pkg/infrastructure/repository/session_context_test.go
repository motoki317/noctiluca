package repository

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/stretchr/testify/require"
)

func TestSessionContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	config := Config{Path: filepath.Join(t.TempDir(), "old.db")}
	db, err := New(ctx, config)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO sessions(key,resume_token) VALUES ('old','token')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = New(ctx, config)
	require.NoError(t, err)
	repo := NewSessionRepository(db)
	saved, err := repo.Get(ctx, "old")
	require.NoError(t, err)
	require.Nil(t, saved.Context)
	saved.Context = &domain.SessionContext{Conversation: "thread", Direct: true, Actor: domain.ActorMember, Workdir: "/tmp/work"}
	require.NoError(t, repo.Save(ctx, saved))
	require.NoError(t, db.Close())
	db, err = New(ctx, config)
	require.NoError(t, err)
	defer db.Close()
	got, err := NewSessionRepository(db).Get(ctx, "old")
	require.NoError(t, err)
	require.Equal(t, saved, got)
}
