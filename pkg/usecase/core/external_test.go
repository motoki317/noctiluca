package core

import (
	"context"
	"errors"
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/stretchr/testify/require"
)

type fakeExternal struct {
	connecting chan bool
	ready      chan struct{}
	running    chan bool
	stopped    chan bool
	err        error
}

func (f *fakeExternal) Connect(ctx context.Context) error {
	f.connecting <- true
	select {
	case <-f.ready:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (f *fakeExternal) Start(ctx context.Context) error {
	f.running <- true
	<-ctx.Done()
	f.stopped <- true
	return nil
}

func TestExternalToolsReadyBeforeFirstTurn(t *testing.T) {
	external := &fakeExternal{connecting: make(chan bool, 1), ready: make(chan struct{}), running: make(chan bool, 1), stopped: make(chan bool, 1)}
	f := setupExternal(t, Config{}, Messaging{Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Users: []string{"member"}}}}, nil, nil, external)
	require.True(t, take(t, external.connecting))
	select {
	case f.messaging.in <- message("test:room", "member", "early"):
		t.Fatal("messaging started before external tools were ready")
	default:
	}
	close(external.ready)
	require.True(t, take(t, external.running))
	f.messaging.in <- message("test:room", "member", "ready")
	session := take(t, f.model.opened)
	_ = take(t, session.inputs)
	require.NoError(t, f.core.Shutdown(t.Context()))
	require.True(t, take(t, external.stopped))
}

func TestExternalStartupFailureDoesNotStopCore(t *testing.T) {
	external := &fakeExternal{connecting: make(chan bool, 1), ready: make(chan struct{}), running: make(chan bool, 1), stopped: make(chan bool, 1), err: errors.New("external startup failed")}
	f := setupExternal(t, Config{}, Messaging{Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Users: []string{"member"}}}}, nil, nil, external)
	take(t, external.connecting)
	close(external.ready)
	take(t, external.running)
	f.messaging.in <- message("test:room", "member", "still working")
	s := take(t, f.model.opened)
	require.Contains(t, take(t, s.inputs).Text, "still working")
}

func (*fakeExternal) Shutdown(context.Context) error { return nil }
