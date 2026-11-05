package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

type oauthFixture struct {
	server          *httptest.Server
	mcp             *mcp.Server
	registrations   atomic.Int32
	refreshes       atomic.Int32
	initializations atomic.Int32
	mu              sync.Mutex
	authorization   url.Values
	redirect        string
	access          string
	refresh         string
	denyRefresh     bool
}

func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()
	f := &oauthFixture{mcp: mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ServerOptions{PageSize: 1}), access: "initial-access", refresh: "initial-refresh"}
	mux := http.NewServeMux()
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return f.mcp }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		access := f.access
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+access {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.server.URL+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read body", 400)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(body, &request) == nil && request.Method == "initialize" {
				f.initializations.Add(1)
			}
		}
		mcpHandler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"resource": f.server.URL + "/mcp", "authorization_servers": []string{f.server.URL}, "scopes_supported": []string{"read", "write"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"issuer": f.server.URL, "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token", "registration_endpoint": f.server.URL + "/register", "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{"read", "write", "offline_access"}, "token_endpoint_auth_methods_supported": []string{"none"}})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var metadata struct {
			RedirectURIs []string `json:"redirect_uris"`
			Scope        string   `json:"scope"`
			GrantTypes   []string `json:"grant_types"`
		}
		if json.NewDecoder(r.Body).Decode(&metadata) != nil || len(metadata.RedirectURIs) != 1 {
			http.Error(w, "invalid registration", 400)
			return
		}
		assert.Equal(t, "read", metadata.Scope)
		assert.Contains(t, metadata.GrantTypes, "refresh_token")
		redirect, err := url.Parse(metadata.RedirectURIs[0])
		assert.NoError(t, err)
		assert.Equal(t, "127.0.0.1", redirect.Hostname())
		f.mu.Lock()
		f.redirect = metadata.RedirectURIs[0]
		f.mu.Unlock()
		f.registrations.Add(1)
		writeJSON(w, map[string]any{"client_id": "registered-client", "token_endpoint_auth_method": "none", "redirect_uris": metadata.RedirectURIs})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		assert.Equal(t, "S256", q.Get("code_challenge_method"))
		assert.NotEmpty(t, q.Get("code_challenge"))
		assert.NotEmpty(t, q.Get("state"))
		assert.Equal(t, "registered-client", q.Get("client_id"))
		assert.Equal(t, "read", q.Get("scope"))
		assert.Equal(t, f.server.URL+"/mcp", q.Get("resource"))
		f.mu.Lock()
		f.authorization = q
		redirect := f.redirect
		f.mu.Unlock()
		http.Redirect(w, r, redirect+"?"+url.Values{"code": {"auth-code"}, "state": {q.Get("state")}}.Encode(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil {
			http.Error(w, "bad form", 400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		assert.Equal(t, "registered-client", r.Form.Get("client_id"))
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			assert.Equal(t, "auth-code", r.Form.Get("code"))
			assert.Equal(t, f.redirect, r.Form.Get("redirect_uri"))
			assert.Equal(t, f.authorization.Get("code_challenge"), oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")))
			assert.Equal(t, f.server.URL+"/mcp", r.Form.Get("resource"))
		case "refresh_token":
			if f.denyRefresh {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid_grant","error_description":"secret-value"}`)
				return
			}
			assert.Equal(t, f.refresh, r.Form.Get("refresh_token"))
			f.refreshes.Add(1)
			f.access = "rotated-access"
			f.refresh = "rotated-refresh"
		default:
			http.Error(w, "bad grant", 400)
			return
		}
		writeJSON(w, map[string]any{"access_token": f.access, "refresh_token": f.refresh, "token_type": "Bearer", "expires_in": 3600})
	})
	return f
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
func browser(t *testing.T) io.Writer {
	t.Helper()
	return writerFunc(func(p []byte) (int, error) {
		response, err := http.Get(strings.TrimSpace(string(p)))
		if err != nil {
			return 0, err
		}
		defer response.Body.Close()
		assert.Equal(t, http.StatusOK, response.StatusCode)
		return len(p), nil
	})
}

func TestLoginPersistRefreshRestart(t *testing.T) {
	f := newOAuthFixture(t)
	path := filepath.Join(t.TempDir(), "oauth.db")
	db, err := repository.New(t.Context(), repository.Config{Path: path})
	require.NoError(t, err)
	repo := repository.NewOAuthTokenRepository(db)
	config := ServerConfig{Name: "tracker", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}
	require.NoError(t, Login(t.Context(), config, repo, browser(t)))
	stored, err := repo.Get(t.Context(), "tracker")
	require.NoError(t, err)
	assert.Equal(t, "initial-refresh", stored.RefreshToken)
	var reg registration
	require.NoError(t, json.Unmarshal(stored.Registration, &reg))
	assert.Equal(t, "registered-client", reg.OAuth.ClientID)
	assert.Equal(t, f.server.URL+"/token", reg.OAuth.Endpoint.TokenURL)
	stored.Expiry = time.Now().Add(-time.Minute)
	require.NoError(t, repo.Save(t.Context(), "tracker", stored))
	require.NoError(t, db.Close())
	db, err = repository.New(t.Context(), repository.Config{Path: path})
	require.NoError(t, err)
	defer db.Close()
	repo = repository.NewOAuthTokenRepository(db)
	source, err := restoreTokens(t.Context(), config, repo)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			token, err := source.Token()
			assert.NoError(t, err)
			if token != nil {
				assert.Equal(t, "rotated-access", token.AccessToken)
			}
		})
	}
	wg.Wait()
	stored, err = repo.Get(t.Context(), "tracker")
	require.NoError(t, err)
	assert.Equal(t, "rotated-refresh", stored.RefreshToken)
	assert.EqualValues(t, 1, f.registrations.Load())
	assert.EqualValues(t, 1, f.refreshes.Load())
	config.URL += "/changed"
	_, err = restoreTokens(t.Context(), config, repo)
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestLoginCancellationAndState(t *testing.T) {
	f := newOAuthFixture(t)
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "oauth.db")})
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithCancel(t.Context())
	var callbackURL string
	output := writerFunc(func(p []byte) (int, error) {
		u, err := url.Parse(strings.TrimSpace(string(p)))
		require.NoError(t, err)
		callbackURL = u.Query().Get("redirect_uri")
		r, err := http.Get(callbackURL + "?state=wrong&code=wrong")
		require.NoError(t, err)
		r.Body.Close()
		assert.Equal(t, http.StatusBadRequest, r.StatusCode)
		cancel()
		return len(p), nil
	})
	repo := repository.NewOAuthTokenRepository(db)
	err = Login(ctx, ServerConfig{Name: "tracker", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}, repo, output)
	require.ErrorIs(t, err, context.Canceled)
	_, err = repo.Get(t.Context(), "tracker")
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = http.Get(callbackURL)
	require.Error(t, err)
}
