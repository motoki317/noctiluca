package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestShutdownAcrossPhases(t *testing.T) {
	var g Group
	ctx, finish, err := g.Begin(t.Context())
	require.NoError(t, err)
	finish()
	require.NoError(t, ctx.Err())
	second, cancel := context.WithCancel(t.Context())
	running, done, err := g.Begin(second)
	require.NoError(t, err)
	cancel()
	select {
	case <-running.Done():
	case <-time.After(time.Second):
		t.Fatal("later phase did not cancel lifetime")
	}
	wait, stop := context.WithTimeout(t.Context(), time.Millisecond)
	defer stop()
	require.ErrorIs(t, g.Shutdown(wait), context.DeadlineExceeded)
	done()
	require.NoError(t, g.Shutdown(t.Context()))
	require.NoError(t, g.Shutdown(t.Context()))
	_, _, err = g.Begin(t.Context())
	require.Error(t, err)
}

func TestShutdownBeforeStart(t *testing.T) {
	var g Group
	require.NoError(t, g.Shutdown(t.Context()))
	_, _, err := g.Begin(t.Context())
	require.Error(t, err)
}
