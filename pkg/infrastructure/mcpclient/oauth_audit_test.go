package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func loginRepo(t *testing.T) domain.OAuthTokenRepository {
	t.Helper()
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "oauth.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return repository.NewOAuthTokenRepository(db)
}

func TestLoginRefreshScopeAndAudience(t *testing.T) {
	f := newOAuthFixture(t)
	f.rootResource, f.tokenRedirect = true, true
	f.noGETChallenge = true
	f.noOffline = true
	f.grantedScope = new("read")
	repo := loginRepo(t)
	config := ServerConfig{Name: "wiki", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}
	require.NoError(t, Login(t.Context(), config, repo, browser(t)))
	for _, scope := range []*string{new("read write"), nil} {
		f.mu.Lock()
		f.grantedScope = scope
		f.mu.Unlock()
		stored, err := repo.Get(t.Context(), config.Name)
		require.NoError(t, err)
		stored.Expiry = time.Now().Add(-time.Minute)
		require.NoError(t, repo.Save(t.Context(), config.Name, stored))
		source, err := restoreTokens(t.Context(), config, repo)
		require.NoError(t, err)
		_, err = source.Token()
		require.NoError(t, err)
		stored, err = repo.Get(t.Context(), config.Name)
		require.NoError(t, err)
		var reg registration
		require.NoError(t, json.Unmarshal(stored.Registration, &reg))
		assert.Equal(t, f.server.URL, reg.Audience)
		assert.Equal(t, []string{"read", "write"}, reg.OAuth.Scopes)
		assert.True(t, stored.Expiry.After(time.Now()))
	}
}

func TestLoginUsesDiscoveredScopes(t *testing.T) {
	f := newOAuthFixture(t)
	f.defaultScopes = true
	require.NoError(t, Login(t.Context(), ServerConfig{Name: "wiki", URL: f.server.URL + "/mcp"}, loginRepo(t), browser(t)))
}

func TestLoginWithAuthorizationCodeOnly(t *testing.T) {
	f := newOAuthFixture(t)
	f.codeOnly, f.noOffline = true, true
	f.refresh = ""
	require.NoError(t, Login(t.Context(), ServerConfig{Name: "tracker", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}, loginRepo(t), browser(t)))
}

func TestLoginUsesPOSTMetadataChallenge(t *testing.T) {
	f := newOAuthFixture(t)
	f.customMetadata, f.noGETChallenge = true, true
	require.NoError(t, Login(t.Context(), ServerConfig{Name: "wiki", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}, loginRepo(t), browser(t)))
}

func TestLoginUsesChallengeScopes(t *testing.T) {
	f := newOAuthFixture(t)
	f.challengeScope = "write"
	require.NoError(t, Login(t.Context(), ServerConfig{Name: "wiki", URL: f.server.URL + "/mcp"}, loginRepo(t), browser(t)))
}

func TestLoginKeepsSDKAuthorizationState(t *testing.T) {
	for _, stepUp := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain403", true: "step-up"}[stepUp], func(t *testing.T) {
			f := newOAuthFixture(t)
			f.loginStepUp, f.loginForbidden = stepUp, !stepUp
			f.challengeScope = "read"
			repo := loginRepo(t)
			err := Login(t.Context(), ServerConfig{Name: "wiki", URL: f.server.URL + "/mcp"}, repo, browser(t))
			if !stepUp {
				require.ErrorContains(t, err, "Forbidden")
				assert.EqualValues(t, 1, f.authorizations.Load())
			} else {
				require.NoError(t, err)
				assert.EqualValues(t, 2, f.authorizations.Load())
				stored, err := repo.Get(t.Context(), "wiki")
				require.NoError(t, err)
				var reg registration
				require.NoError(t, json.Unmarshal(stored.Registration, &reg))
				assert.ElementsMatch(t, []string{"read", "write", "offline_access"}, reg.OAuth.Scopes)
			}
			assert.Equal(t, f.authorizations.Load(), f.metadataGets.Load(), "discover once per authorization")
		})
	}
}

func TestReloginReplacesOnlyAfterSuccess(t *testing.T) {
	f := newOAuthFixture(t)
	repo := loginRepo(t)
	config := ServerConfig{Name: "wiki", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}
	require.NoError(t, Login(t.Context(), config, repo, browser(t)))
	before, err := repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	f.denyCode = true
	require.Error(t, Login(t.Context(), config, repo, browser(t)))
	after, err := repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	f.denyCode = false
	f.mu.Lock()
	f.access, f.refresh = "replacement-access", "replacement-refresh"
	f.mu.Unlock()
	require.NoError(t, Login(t.Context(), config, repo, browser(t)))
	after, err = repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	assert.Equal(t, "replacement-refresh", after.RefreshToken)
	assert.NotEqual(t, before.Registration, after.Registration)
	source, err := restoreTokens(t.Context(), config, repo)
	require.NoError(t, err)
	token, err := source.Token()
	require.NoError(t, err)
	assert.Equal(t, "replacement-access", token.AccessToken)
}

func TestOldRefreshCannotOverwriteRelogin(t *testing.T) {
	f := newOAuthFixture(t)
	repo := loginRepo(t)
	config := ServerConfig{Name: "wiki", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}
	require.NoError(t, Login(t.Context(), config, repo, browser(t)))
	stored, err := repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	stored.Expiry = time.Now().Add(-time.Minute)
	require.NoError(t, repo.Save(t.Context(), config.Name, stored))
	old, err := restoreTokens(t.Context(), config, repo)
	require.NoError(t, err)
	require.NoError(t, Login(t.Context(), config, repo, browser(t)))
	replacement, err := repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	_, err = old.Token()
	require.ErrorContains(t, err, "stored login changed")
	actual, err := repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	assert.Equal(t, replacement, actual)
}

func TestLoginRejectsWrongOrMissingIssuer(t *testing.T) {
	for _, advertised := range []bool{false, true} {
		for _, wrong := range []bool{false, true} {
			if !advertised && !wrong {
				continue
			}
			t.Run(strings.Join([]string{map[bool]string{true: "advertised", false: "unadvertised"}[advertised], map[bool]string{true: "wrong", false: "missing"}[wrong]}, "/"), func(t *testing.T) {
				f := newOAuthFixture(t)
				f.advertiseIss, f.wrongIss = advertised, wrong
				repo := loginRepo(t)
				err := Login(t.Context(), ServerConfig{Name: "wiki", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}, repo, browser(t))
				require.ErrorContains(t, err, "iss")
				_, err = repo.Get(t.Context(), "wiki")
				require.ErrorIs(t, err, domain.ErrNotFound)
			})
		}
	}
}

func TestHTTPRejectsPrivateDestinationsAndRedirects(t *testing.T) {
	for _, address := range []string{"10.0.0.1", "169.254.169.254", "100.64.0.1", "127.0.0.1", "::1", "::ffff:127.0.0.1", "fd00::1", "fe80::1", "0.0.0.0", "0.0.0.1", "198.18.0.1", "224.0.0.1"} {
		assert.False(t, publicIP(netip.MustParseAddr(address)), address)
	}
	assert.True(t, publicIP(netip.MustParseAddr("8.8.8.8")))
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Location", "http://127.0.0.1:1/credentials")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	for _, endpoint := range []string{server.URL, "http://localhost:" + strings.Split(server.URL, ":")[2], "https://10.0.0.1/token", "https://169.254.169.254/register", "https://100.64.0.1/token"} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
		require.NoError(t, err)
		response, err := oauthHTTPClient("https://remote.example/mcp").Do(req)
		cancel()
		require.Error(t, err, endpoint)
		assert.Nil(t, response)
		assert.NotErrorIs(t, err, context.DeadlineExceeded)
	}
	assert.Zero(t, calls)
	response, err := oauthHTTPClient(server.URL).Get(server.URL)
	require.ErrorContains(t, err, "unsafe MCP HTTP URL")
	if response != nil {
		response.Body.Close()
	}
	assert.Equal(t, 1, calls)
}
