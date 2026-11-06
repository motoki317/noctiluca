package mcpclient

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The SDK can request authorization more than once in one login; the second
// request must still reach a listening callback server.
func TestCallbackServerServesRepeatedAuthorization(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	callbacks := serveCallbacks(listener)
	defer callbacks.close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	for _, state := range []string{"first", "second"} {
		output := writerFunc(func(p []byte) (int, error) {
			u, err := url.Parse(strings.TrimSpace(string(p)))
			require.NoError(t, err)
			go func() {
				r, err := http.Get(redirect + "?state=" + u.Query().Get("state") + "&code=c-" + state)
				if err == nil {
					r.Body.Close()
				}
			}()
			return len(p), nil
		})
		result, err := callbacks.receive(context.Background(), "https://auth.example/authorize?state="+state, output, nil)
		require.NoError(t, err)
		require.Equal(t, "c-"+state, result.Code)
	}
}
