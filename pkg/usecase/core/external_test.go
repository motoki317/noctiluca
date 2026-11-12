package core

import (
	"context"
	"errors"
	"fmt"
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
	for _, connectErr := range []error{nil, errors.New("external startup failed")} {
		t.Run(fmt.Sprint(connectErr), func(t *testing.T) {
			external := &fakeExternal{connecting: make(chan bool, 1), ready: make(chan struct{}), running: make(chan bool, 1), stopped: make(chan bool, 1), err: connectErr}
			f := setup(t, Config{}, []Messaging{{Rules: map[domain.ConversationRef]domain.AcceptRule{"test:room": {Users: []string{"member"}}}}}, fixtureOptions{external: external})
			take(t, external.connecting)
			select {
			case f.messaging.in <- message("test:room", "member", "early"):
				t.Fatal("messaging started before external tools were ready")
			default:
			}
			close(external.ready)
			take(t, external.running)
			f.messaging.in <- message("test:room", "member", "ready")
			session := take(t, f.model.opened)
			require.Contains(t, take(t, session.inputs).Text, "ready")
			require.NoError(t, f.core.Shutdown(t.Context()))
			take(t, external.stopped)
		})
	}
}

func (*fakeExternal) Shutdown(context.Context) error { return nil }
