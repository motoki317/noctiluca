package repository_test

import (
	"context"
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/stretchr/testify/require"
)

func TestListSessions(t *testing.T) {
	db := openDB(t)
	sessions := repository.NewSessionRepository(db)
	listed, err := sessions.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, listed)
	want := []domain.Session{
		{Key: "conversation", ResumeToken: "saved", Context: &domain.SessionContext{Conversation: "room", Direct: true, Workdir: "/work"}},
		{Key: "empty", Context: &domain.SessionContext{Conversation: "new"}},
		{Key: "no-context", ResumeToken: "legacy"},
		{Key: "task:daily", ResumeToken: "task-token", Context: &domain.SessionContext{TaskName: "daily", Workdir: "/task"}},
	}
	for _, session := range want {
		require.NoError(t, sessions.Save(t.Context(), session))
	}
	listed, err = sessions.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, listed)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = sessions.List(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = db.ExecContext(t.Context(), `UPDATE sessions SET context_json='"invalid"' WHERE key='empty'`)
	require.NoError(t, err)
	_, err = sessions.List(t.Context())
	require.Error(t, err)
}
