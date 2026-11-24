package core

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func pickEventAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	return addr
}

func TestCoreEventsEndpoint(t *testing.T) {
	addr := pickEventAddress(t)
	const token = "test-event-token"
	f := setup(t, Config{EventsListen: addr, EventsToken: token}, []Messaging{{Owner: "owner"}}, fixtureOptions{})
	endpoint := "http://" + addr + "/events/calendar"
	client := &http.Client{Timeout: time.Second}
	t.Cleanup(client.CloseIdleConnections)

	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader("new invitation"))
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(r)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	model := take(t, f.model.opened)
	require.Equal(t, domain.MainSessionKey, model.config.Key)
	input := take(t, model.inputs)
	require.Regexp(t, `^Event calendar \| external \| \d{2}:\d{2}:\d{2} .+\n\nnew invitation$`, input.Text)

	require.NoError(t, f.core.Shutdown(t.Context()))
	response, err = client.Get(endpoint)
	if response != nil {
		response.Body.Close()
	}
	require.Error(t, err)
}

type shutdownErrorExternal struct {
	domain.ExternalTools
	err error
}

func (s shutdownErrorExternal) Shutdown(context.Context) error { return s.err }

func TestCoreShutdownBeforeStart(t *testing.T) {
	addr := pickEventAddress(t)
	externalErr := errors.New("external shutdown failed")
	c, err := New(Config{EventsListen: addr, EventsToken: "test-event-token"},
		[]Messaging{{Name: "test", Owner: "owner", Adapter: &fakeMessaging{}}},
		&fakeModel{}, &fakeHub{tools: make(map[string]domain.Tool)}, &unusedEvents{}, &unusedSessions{},
		nil, &unusedSchedules{}, &unusedWatches{}, shutdownErrorExternal{err: externalErr}, nil, &unusedInbox{})
	require.NoError(t, err)
	require.ErrorIs(t, c.Shutdown(t.Context()), externalErr)
	require.ErrorContains(t, c.Start(t.Context()), "core is stopped")

	listener, err := net.Listen("tcp4", addr)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
}
