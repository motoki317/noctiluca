package mcpclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPRedirects(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/mcp":
			http.Redirect(w, r, "/mcp/", http.StatusTemporaryRedirect)
		case "/mcp/":
			body, _ := io.ReadAll(r.Body)
			assert.Equal(t, "POST", r.Method)
			assert.Equal(t, "payload", string(body))
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/invalid":
			http.Redirect(w, r, "https://user:secret@example.com/", http.StatusFound)
		}
	}))
	defer s.Close()
	client := oauthHTTPClient(s.URL)
	resp, err := client.Post(s.URL+"/mcp", "application/json", strings.NewReader("payload"))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.EqualValues(t, 2, calls.Load())
	for _, path := range []string{"/loop", "/invalid"} {
		resp, err = client.Get(s.URL + path)
		require.Error(t, err)
		if resp != nil {
			resp.Body.Close()
		}
	}
	secure, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	insecure, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1", nil)
	require.ErrorContains(t, client.CheckRedirect(insecure, []*http.Request{secure}), "downgrades HTTPS")
	for _, pair := range [][2]string{{"http://localhost", "http://localhost:80"}, {"https://127.0.0.1", "https://127.0.0.1:443"}, {"http://[::1]", "http://[::1]:80"}} {
		a, _ := url.Parse(pair[0])
		b, _ := url.Parse(pair[1])
		assert.Equal(t, dialAddress(a), dialAddress(b))
	}
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
