package mcpclient

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPRedirects(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/invalid":
			http.Redirect(w, r, "https://user:secret@example.com/", http.StatusFound)
		}
	}))
	defer s.Close()
	client := oauthHTTPClient(s.URL)
	for _, path := range []string{"/loop", "/invalid"} {
		resp, err := client.Get(s.URL + path)
		require.Error(t, err)
		if resp != nil {
			resp.Body.Close()
		}
	}
	secure, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	insecure, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1", nil)
	require.ErrorContains(t, client.CheckRedirect(insecure, []*http.Request{secure}), "downgrades HTTPS")
}

func TestHTTPProxyFromEnvironment(t *testing.T) {
	// ProxyFromEnvironment caches environment variables, so set them before process startup.
	if os.Getenv("NOCTILUCA_PROXY_TEST") != "" {
		resp, err := oauthHTTPClient("https://remote.example/mcp").Get("https://remote.example/mcp")
		if resp != nil {
			resp.Body.Close()
		}
		require.ErrorContains(t, err, "Bad Gateway")
		return
	}
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, http.MethodConnect, r.Method)
		assert.Equal(t, "remote.example:443", r.Host)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHTTPProxyFromEnvironment$")
	cmd.Env = append(os.Environ(), "NOCTILUCA_PROXY_TEST=1", "HTTPS_PROXY="+proxy.URL, "NO_PROXY=", "no_proxy=")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	assert.EqualValues(t, 1, calls.Load())
}
