package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mcpEntry = `mcp_servers:
  - name: tracker
    url: https://tracker.example/mcp
`

const preregisteredClient = `    client_id: example-client
    authorization_server: https://auth.tracker.example
`

func TestMCPConfigAndLoginSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("mcp_servers:\n  - name: tracker\n    url: https://tracker.example/mcp\n    scopes: [read, offline_access]\n    tools: [search]\n  - name: wiki\n    url: https://wiki.example/mcp\n"), 0o600))
	_, err := loadConfig(path)
	require.NoError(t, err)
	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--config", path, "mcp-login", "missing"})
	require.ErrorContains(t, cmd.Execute(), "not configured")
	assert.Empty(t, out.String())
}

func TestPreregisteredMCPConfig(t *testing.T) {
	t.Setenv("TEST_APP_TOKEN", "app-token")
	t.Setenv("TEST_BOT_TOKEN", "bot-token")
	messaging := slackEntry + `    slack:
      app_token: {env: TEST_APP_TOKEN}
      bot_token: {env: TEST_BOT_TOKEN}
`
	for _, test := range []struct {
		name    string
		file    string
		wantErr string
	}{
		{name: "missing secret is optional for run", file: "secrets/client"},
		{name: "secret must be outside workdir", file: "work/client", wantErr: "model.workdir must be separate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, messaging+mcpEntry+preregisteredClient+"    client_secret: {file: "+test.file+"}\n")
			config, err := loadConfig(path)
			require.NoError(t, err)
			require.Len(t, config.MCPServers, 1)
			server := config.MCPServers[0]
			assert.Equal(t, "example-client", server.ClientID)
			assert.Equal(t, "https://auth.tracker.example", server.AuthorizationServer)
			assert.Equal(t, filepath.Join(filepath.Dir(path), test.file), server.ClientSecretRef.File)
			_, _, err = config.validateRun()
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestInvalidPreregisteredMCPConfig(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings string
		want     string
	}{
		{"secret env without ID", "    client_secret: {env: secret-value}\n", "client_secret requires client_id"},
		{"secret file without ID", "    client_secret: {file: secret-value}\n", "client_secret requires client_id"},
		{"issuer without ID", "    authorization_server: https://auth.tracker.example\n", "authorization_server requires client_id"},
		{"ID without issuer", "    client_id: example-client\n", "requires an authorization_server URL"},
		{"invalid issuer", "    client_id: example-client\n    authorization_server: https://secret-value@auth.tracker.example\n", "requires an authorization_server URL"},
		{"unknown secret key", preregisteredClient + "    client_secret: {secret-value: {}}\n", "write client_secret as"},
		{"inline secret", preregisteredClient + "    client_secret: secret-value\n", "client_secret"},
		{"inline field name", preregisteredClient + "    clientsecret: secret-value\n", "clientsecret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadConfig(writeConfig(t, mcpEntry+test.settings))
			require.ErrorContains(t, err, test.want)
			assert.NotContains(t, err.Error(), "secret-value")
		})
	}
}

func TestMCPLoginReadsSecretReference(t *testing.T) {
	for _, ref := range []string{"{env: TEST_MCP_SECRET}", "{file: client-secret}"} {
		t.Run(ref, func(t *testing.T) {
			const secret = "test-secret with spaces"
			t.Setenv("TEST_MCP_SECRET", secret)
			path := writeConfig(t, mcpEntry+preregisteredClient+"    client_secret: "+ref+"\n")
			require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "client-secret"), []byte(secret+"\n"), 0600))
			cmd := newRootCommand()
			cmd.SetArgs([]string{"--config", path, "mcp-login", "tracker"})
			err := cmd.Execute()
			require.ErrorContains(t, err, "client_secret: token contains whitespace")
			assert.NotContains(t, err.Error(), secret)
		})
	}
}

func TestMCPLoginPassesClientSecret(t *testing.T) {
	const secret = "test-client-secret"
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"code_challenge_methods_supported":["S256"],"token_endpoint_auth_methods_supported":["client_secret_post"]}`, server.URL, server.URL+"/authorize", server.URL+"/token")
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=test&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, secret, r.PostForm.Get("client_secret"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"test-access","token_type":"Bearer"}`)
	})
	previous := openBrowser
	t.Cleanup(func() { openBrowser = previous })
	openBrowser = func(authURL string) error {
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(authURL)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
	path := writeConfig(t, fmt.Sprintf("mcp_servers:\n  - {name: tracker, url: %s/mcp, client_id: client, authorization_server: %s, client_secret: {file: client-secret}}\n", server.URL, server.URL))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "client-secret"), []byte(secret+"\n"), 0600))
	cmd := newRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--config", path, "mcp-login", "tracker"})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, cmd.ExecuteContext(ctx))
}
