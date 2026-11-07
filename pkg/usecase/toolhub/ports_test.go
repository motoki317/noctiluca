package toolhub

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPortsAndShutdown(t *testing.T) {
	for _, phase := range []string{"before start", "after start"} {
		t.Run(phase, func(t *testing.T) {
			created, err := New(Config{})
			require.NoError(t, err)
			h := created.(*hub)
			var done chan error
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				require.NoError(t, h.Shutdown(ctx))
				if done != nil {
					require.NoError(t, <-done)
				}
			})
			ep, err := h.OpenSession("session")
			require.NoError(t, err)
			parsed, err := url.Parse(ep.URL)
			require.NoError(t, err)
			require.Equal(t, "127.0.0.1", parsed.Hostname())
			require.NotEqual(t, "0", parsed.Port())
			port, err := strconv.Atoi(parsed.Port())
			require.NoError(t, err)
			_, err = New(Config{Port: port})
			require.Error(t, err)
			if phase == "after start" {
				done = make(chan error, 1)
				go func() { done <- h.Start(context.Background()) }()
				transport := &http.Transport{DisableKeepAlives: true}
				defer transport.CloseIdleConnections()
				client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ep.URL, nil)
				require.NoError(t, err)
				resp, err := client.Do(req)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
			}
			require.NoError(t, h.Shutdown(t.Context()))
			// Check our listener: another process can claim the port as soon as Shutdown releases it.
			require.ErrorIs(t, h.listener.Close(), net.ErrClosed)
			require.NoError(t, h.Shutdown(t.Context()))
			_, err = h.OpenSession("closed")
			require.Error(t, err)
			require.Error(t, h.AddSource("closed", nil))
		})
	}
	for _, port := range []int{-1, 65536} {
		_, err := New(Config{Port: port})
		require.Error(t, err)
	}
}
