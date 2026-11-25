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
	server            *httptest.Server
	mcp               *mcp.Server
	registrations     atomic.Int32
	authorizations    atomic.Int32
	tokenRequests     atomic.Int32
	metadataGets      atomic.Int32
	preregistered     bool
	clientID          string
	clientSecret      string
	loginStepUp       bool
	loginForbidden    bool
	openHandshake     bool
	anonymousClosed   atomic.Bool
	refreshes         atomic.Int32
	refreshStatus     atomic.Int32
	initializations   atomic.Int32
	bearerInits       atomic.Int32
	mu                sync.Mutex
	authorization     url.Values
	redirect          string
	access            string
	refresh           string
	sendIss           bool // returns iss without advertising RFC 9207 support, as Slite does
	noOffline         bool
	rootResource      bool
	omitPRM           bool
	grantedScope      *string
	wrongIss          bool
	advertiseIss      bool
	exchangeError     string
	runtimeStatus     atomic.Int32
	insufficientScope bool
	defaultScopes     bool
	codeOnly          bool
	challengeScope    string
}

func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()
	f := &oauthFixture{mcp: mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ServerOptions{PageSize: 1}), clientID: "registered-client", access: "initial-access", refresh: "initial-refresh"}
	mux := http.NewServeMux()
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return f.mcp }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		access := f.access
		f.mu.Unlock()
		bearer := r.Header.Get("Authorization")
		if bearer != "Bearer "+access && (!f.openHandshake || bearer != "") {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.server.URL+`/.well-known/oauth-protected-resource/mcp"`)
			if f.challengeScope != "" {
				w.Header().Set("WWW-Authenticate", w.Header().Get("WWW-Authenticate")+`, scope="`+f.challengeScope+`"`)
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.authorizations.Load() == 1 && (f.loginStepUp || f.loginForbidden) {
			if f.loginStepUp {
				w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="write", resource_metadata="`+f.server.URL+`/.well-known/oauth-protected-resource/mcp"`)
			}
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if status := f.runtimeStatus.Load(); status != 0 {
			if f.insufficientScope {
				w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="write"`)
			}
			w.WriteHeader(int(status))
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
				if bearer != "" {
					f.bearerInits.Add(1)
				}
			}
		}
		if f.openHandshake && r.Method == http.MethodDelete && bearer == "" && r.Header.Get("Mcp-Session-Id") != "" {
			f.anonymousClosed.Store(true)
		}
		mcpHandler.ServeHTTP(w, r)
	})
	prmHandler := func(w http.ResponseWriter, r *http.Request) {
		if f.rootResource || f.omitPRM {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		issuer := f.server.URL
		if f.preregistered {
			issuer += "/"
		}
		writeJSON(w, map[string]any{"resource": f.server.URL + "/mcp", "authorization_servers": []string{issuer}, "scopes_supported": []string{"read", "write"}})
	}
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", prmHandler)
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		if f.omitPRM {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"resource": f.server.URL, "authorization_servers": []string{f.server.URL}, "scopes_supported": []string{"read", "write"}})
	})
	authMetadata := func(w http.ResponseWriter, r *http.Request) {
		f.metadataGets.Add(1)
		scopes := []string{"read", "write"}
		if !f.noOffline {
			scopes = append(scopes, "offline_access")
		}
		var grants []string
		if f.codeOnly {
			grants = []string{"authorization_code"}
		}
		metadata := map[string]any{"issuer": f.server.URL, "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token", "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": scopes, "authorization_response_iss_parameter_supported": f.advertiseIss, "grant_types_supported": grants}
		if f.preregistered {
			metadata["token_endpoint_auth_methods_supported"] = []string{"client_secret_post", "client_secret_basic"}
		} else {
			metadata["registration_endpoint"] = f.server.URL + "/register"
			metadata["token_endpoint_auth_methods_supported"] = []string{"none"}
		}
		writeJSON(w, metadata)
	}
	mux.HandleFunc("/.well-known/oauth-authorization-server", authMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server/{$}", authMetadata)
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		f.registrations.Add(1)
		var metadata struct {
			RedirectURIs []string `json:"redirect_uris"`
			Scope        string   `json:"scope"`
			GrantTypes   []string `json:"grant_types"`
		}
		if json.NewDecoder(r.Body).Decode(&metadata) != nil || len(metadata.RedirectURIs) != 1 {
			http.Error(w, "invalid registration", 400)
			return
		}
		assert.Empty(t, metadata.Scope, "let the server choose registration defaults")
		assert.Equal(t, []string{"authorization_code", "refresh_token"}, metadata.GrantTypes)
		redirect, err := url.Parse(metadata.RedirectURIs[0])
		assert.NoError(t, err)
		assert.Equal(t, "127.0.0.1", redirect.Hostname())
		f.mu.Lock()
		f.redirect = metadata.RedirectURIs[0]
		f.mu.Unlock()
		writeJSON(w, map[string]any{"client_id": f.clientID, "token_endpoint_auth_method": "none", "redirect_uris": metadata.RedirectURIs})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		round := f.authorizations.Add(1)
		if f.openHandshake {
			assert.True(t, f.anonymousClosed.Load(), "close the anonymous session before consent")
		}
		q := r.URL.Query()
		assert.Equal(t, "S256", q.Get("code_challenge_method"))
		assert.NotEmpty(t, q.Get("code_challenge"))
		assert.NotEmpty(t, q.Get("state"))
		assert.Equal(t, f.clientID, q.Get("client_id"))
		assert.NotContains(t, q, "client_secret")
		if f.clientSecret != "" {
			assert.NotContains(t, r.URL.RawQuery, url.QueryEscape(f.clientSecret))
		}
		scopes := []string{"read"}
		if f.defaultScopes {
			scopes = append(scopes, "write")
		}
		if f.challengeScope != "" {
			scopes = strings.Fields(f.challengeScope)
		}
		if !f.noOffline {
			scopes = append(scopes, "offline_access")
		}
		if f.loginStepUp && round > 1 {
			scopes = append(scopes, "write")
		}
		assert.ElementsMatch(t, scopes, strings.Fields(q.Get("scope")))
		assert.Equal(t, f.resource(), q.Get("resource"))
		f.mu.Lock()
		f.authorization = q
		if f.preregistered {
			f.redirect = q.Get("redirect_uri")
		}
		redirect := f.redirect
		f.mu.Unlock()
		values := url.Values{"code": {"auth-code"}, "state": {q.Get("state")}}
		if f.sendIss {
			values.Set("iss", f.server.URL)
		}
		if f.wrongIss {
			values.Set("iss", "https://wrong.example")
		}
		http.Redirect(w, r, redirect+"?"+values.Encode(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenRequests.Add(1)
		if r.ParseForm() != nil {
			http.Error(w, "bad form", 400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		assert.Equal(t, f.clientID, r.PostForm.Get("client_id"))
		assert.Empty(t, r.Header.Get("Authorization"))
		if f.clientSecret == "" {
			assert.NotContains(t, r.PostForm, "client_secret")
		} else {
			assert.Equal(t, f.clientSecret, r.PostForm.Get("client_secret"))
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if f.exchangeError != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]string{"error": f.exchangeError, "error_description": r.PostForm.Get("client_secret")})
				return
			}
			assert.Equal(t, "auth-code", r.Form.Get("code"))
			assert.Equal(t, f.redirect, r.Form.Get("redirect_uri"))
			assert.Equal(t, f.authorization.Get("code_challenge"), oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")))
			assert.Equal(t, f.resource(), r.Form.Get("resource"))
		case "refresh_token":
			if status := f.refreshStatus.Load(); status != 0 {
				w.WriteHeader(int(status))
				fmt.Fprint(w, `{"error":"server_error","error_description":"secret-value"}`)
				return
			}
			assert.Equal(t, f.resource(), r.Form.Get("resource"))
			assert.Equal(t, f.refresh, r.Form.Get("refresh_token"))
			round := f.refreshes.Add(1)
			f.access = "rotated-access"
			f.refresh = "rotated-refresh"
			if round > 1 {
				f.access = fmt.Sprintf("rotated-access-%d", round)
				f.refresh = fmt.Sprintf("rotated-refresh-%d", round)
			}
		default:
			http.Error(w, "bad grant", 400)
			return
		}
		token := map[string]any{"access_token": f.access, "refresh_token": f.refresh, "token_type": "Bearer", "expires_in": 3600}
		if f.grantedScope != nil {
			token["scope"] = *f.grantedScope
		}
		writeJSON(w, token)
	})
	return f
}

func (f *oauthFixture) resource() string {
	if f.rootResource {
		return f.server.URL
	}
	return f.server.URL + "/mcp"
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

func TestLoginPersistsAndRefreshes(t *testing.T) {
	f := newOAuthFixture(t)
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "oauth.db")})
	require.NoError(t, err)
	defer db.Close()
	repo := repository.NewOAuthTokenRepository(db)
	config := ServerConfig{Name: "tracker", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}
	require.NoError(t, Login(t.Context(), config, "", repo, browser(t), nil))
	stored, err := repo.Get(t.Context(), "tracker")
	require.NoError(t, err)
	assert.Equal(t, "initial-refresh", stored.RefreshToken)
	var reg registration
	require.NoError(t, json.Unmarshal(stored.Registration, &reg))
	assert.ElementsMatch(t, []string{"read", "offline_access"}, reg.OAuth.Scopes)
	stored.Expiry = time.Now().Add(-time.Minute)
	require.NoError(t, repo.Save(t.Context(), "tracker", stored))
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

func newPreregisteredOAuthFixture(t *testing.T, secret string) (*oauthFixture, ServerConfig) {
	t.Helper()
	f := newOAuthFixture(t)
	f.preregistered, f.noOffline, f.advertiseIss, f.sendIss = true, true, true, true
	f.clientID, f.clientSecret = "preregistered-client", secret
	return f, ServerConfig{
		Name: "tracker", URL: f.server.URL + "/mcp", Scopes: []string{"read"},
		ClientID: f.clientID, AuthorizationServer: f.server.URL,
	}
}

func TestPreregisteredLoginPersistsAndRefreshes(t *testing.T) {
	f, config := newPreregisteredOAuthFixture(t, "test-client-secret")
	repo := loginRepo(t)
	require.NoError(t, Login(t.Context(), config, f.clientSecret, repo, browser(t), nil))
	stored, err := repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	assert.Equal(t, "initial-refresh", stored.RefreshToken)
	stored.Expiry = time.Now().Add(-time.Minute)
	require.NoError(t, repo.Save(t.Context(), config.Name, stored))
	source, err := restoreTokens(t.Context(), config, repo)
	require.NoError(t, err)
	token, err := source.Token()
	require.NoError(t, err)
	assert.Equal(t, "rotated-access", token.AccessToken)
	stored, err = repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	assert.Equal(t, "rotated-refresh", stored.RefreshToken)
	require.NoError(t, source.refreshRejected("Bearer "+token.AccessToken))
	stored, err = repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	assert.Equal(t, "rotated-access-2", stored.AccessToken)
	assert.Equal(t, "rotated-refresh-2", stored.RefreshToken)
	assert.EqualValues(t, 2, f.refreshes.Load())
	assert.EqualValues(t, 3, f.tokenRequests.Load())
	assert.EqualValues(t, 1, f.metadataGets.Load())
	assert.Zero(t, f.registrations.Load())
}

func TestPreregisteredPublicClientLogin(t *testing.T) {
	f, config := newPreregisteredOAuthFixture(t, "")
	repo := loginRepo(t)
	require.NoError(t, Login(t.Context(), config, "", repo, browser(t), nil))
	_, err := repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	assert.EqualValues(t, 1, f.tokenRequests.Load())
	assert.Zero(t, f.registrations.Load())
}

func TestPreregisteredLoginWithOpenHandshake(t *testing.T) {
	for _, test := range []struct {
		name      string
		forbidden bool
	}{
		{name: "authenticated handshake succeeds"},
		{name: "authenticated handshake fails", forbidden: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, config := newPreregisteredOAuthFixture(t, "test-client-secret")
			f.openHandshake, f.loginForbidden = true, test.forbidden
			repo := loginRepo(t)

			err := Login(t.Context(), config, f.clientSecret, repo, browser(t), nil)
			assert.EqualValues(t, 1, f.authorizations.Load())
			assert.EqualValues(t, 1, f.tokenRequests.Load())
			assert.Zero(t, f.registrations.Load())
			if test.forbidden {
				require.ErrorContains(t, err, "Forbidden")
				_, err = repo.Get(t.Context(), config.Name)
				require.ErrorIs(t, err, domain.ErrNotFound)
				return
			}
			require.NoError(t, err)
			stored, err := repo.Get(t.Context(), config.Name)
			require.NoError(t, err)
			assert.Equal(t, "initial-access", stored.AccessToken)
			assert.EqualValues(t, 2, f.initializations.Load())
			assert.EqualValues(t, 1, f.bearerInits.Load())
		})
	}
}

func TestDynamicLoginWithOpenHandshake(t *testing.T) {
	f := newOAuthFixture(t)
	f.openHandshake = true
	config := ServerConfig{Name: "tracker", URL: f.server.URL + "/mcp"}
	repo := loginRepo(t)

	err := Login(t.Context(), config, "", repo, browser(t), nil)
	require.ErrorContains(t, err, "MCP server did not request OAuth authorization")
	assert.Contains(t, err.Error(), "needs client_id and authorization_server (docs/running.md, step 7)")
	assert.EqualValues(t, 1, f.initializations.Load())
	assert.Zero(t, f.registrations.Load())
	assert.Zero(t, f.authorizations.Load())
	assert.Zero(t, f.tokenRequests.Load())
	_, err = repo.Get(t.Context(), config.Name)
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestPreregisteredLoginRejectsMismatchedIssuer(t *testing.T) {
	for _, test := range []struct {
		name          string
		openHandshake bool
		omitPRM       bool
	}{
		{name: "discovered issuer"},
		{name: "guessed issuer", openHandshake: true, omitPRM: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, config := newPreregisteredOAuthFixture(t, "test-client-secret")
			f.openHandshake, f.omitPRM = test.openHandshake, test.omitPRM
			config.AuthorizationServer = "https://other.example"
			repo := loginRepo(t)
			var output bytes.Buffer
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			err := Login(ctx, config, f.clientSecret, repo, &output, func(string) error {
				cancel()
				return nil
			})
			require.ErrorContains(t, err, "does not match pre-registered credentials issuer")
			assert.Empty(t, output.String())
			assert.Positive(t, f.metadataGets.Load())
			assert.Zero(t, f.registrations.Load())
			assert.Zero(t, f.authorizations.Load())
			assert.Zero(t, f.tokenRequests.Load())
			_, err = repo.Get(t.Context(), config.Name)
			require.ErrorIs(t, err, domain.ErrNotFound)
		})
	}
}

func TestPreregisteredLoginHidesSecretInExchangeError(t *testing.T) {
	for _, test := range []struct {
		name   string
		secret string
		code   string
		detail string
	}{
		{name: "plain secret", secret: "test-client-secret", code: "invalid_grant", detail: ", invalid_grant"},
		{name: "quoted secret", secret: `test-"client\secret`, code: "invalid_grant", detail: ", invalid_grant"},
		{name: "resource rejected", secret: "test-client-secret", code: "invalid_target", detail: ", invalid_target"},
		{name: "encoded echo in error code", secret: `test-"client\secret`, code: url.QueryEscape(`test-"client\secret`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, config := newPreregisteredOAuthFixture(t, test.secret)
			f.exchangeError = test.code
			err := Login(t.Context(), config, test.secret, loginRepo(t), browser(t), nil)
			require.EqualError(t, err, "MCP login failed: token request rejected (400 Bad Request"+test.detail+")")
			assert.NotContains(t, err.Error(), test.secret)
			assert.Positive(t, f.tokenRequests.Load())
		})
	}
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
	err = Login(ctx, ServerConfig{Name: "tracker", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}, "", repo, output, nil)
	require.ErrorIs(t, err, context.Canceled)
	_, err = repo.Get(t.Context(), "tracker")
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = http.Get(callbackURL)
	require.Error(t, err)
}

// ClickUp issues long-lived access tokens without a refresh token.
func TestLoginWithoutRefreshToken(t *testing.T) {
	f := newOAuthFixture(t)
	f.codeOnly, f.noOffline = true, true
	f.refresh = ""
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "oauth.db")})
	require.NoError(t, err)
	defer db.Close()
	repo := repository.NewOAuthTokenRepository(db)
	config := ServerConfig{Name: "tracker", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}
	require.NoError(t, Login(t.Context(), config, "", repo, browser(t), nil))
	stored, err := repo.Get(t.Context(), "tracker")
	require.NoError(t, err)
	assert.Equal(t, "initial-access", stored.AccessToken)
	assert.Empty(t, stored.RefreshToken)
	source, err := restoreTokens(t.Context(), config, repo)
	require.NoError(t, err)
	token, err := source.Token()
	require.NoError(t, err)
	assert.Equal(t, "initial-access", token.AccessToken)
	stored.Expiry = time.Now().Add(-time.Minute)
	require.NoError(t, repo.Save(t.Context(), "tracker", stored))
	source, err = restoreTokens(t.Context(), config, repo)
	require.NoError(t, err)
	_, err = source.Token()
	require.ErrorContains(t, err, "run mcp-login tracker")
}
