package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/usecase/toolhub"
)

func TestRuntimeAuthFailureDisablesAndNotifiesOnce(t *testing.T) {
	for _, scenario := range []string{"401", "401-no-refresh", "step-up", "expiry", "expired-startup"} {
		t.Run(scenario, func(t *testing.T) {
			f := newOAuthFixture(t)
			f.insufficientScope = scenario == "step-up"
			if scenario == "expiry" || scenario == "expired-startup" || scenario == "401-no-refresh" {
				f.refresh = ""
			}
			f.mcp.AddTool(&mcp.Tool{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
			db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
			require.NoError(t, err)
			defer db.Close()
			repo, inbox := repository.NewOAuthTokenRepository(db), repository.NewInboxRepository(db)
			config := ServerConfig{Name: "wiki", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}
			require.NoError(t, Login(t.Context(), config, repo, browser(t), nil))
			stored, err := repo.Get(t.Context(), config.Name)
			require.NoError(t, err)
			if scenario == "expired-startup" {
				stored.Expiry = time.Now().Add(-time.Minute)
				require.NoError(t, repo.Save(t.Context(), config.Name, stored))
			}
			hub, err := toolhub.New(toolhub.Config{})
			require.NoError(t, err)
			hubDone := make(chan error, 1)
			go func() { hubDone <- hub.Start(t.Context()) }()
			defer func() { require.NoError(t, hub.Shutdown(t.Context())); require.NoError(t, <-hubDone) }()
			m, err := New([]ServerConfig{config}, repo, inbox, hub, slog.New(slog.DiscardHandler))
			require.NoError(t, err)
			require.NoError(t, m.Connect(t.Context()))
			r := m.(*manager).remotes[0]
			// Establish a short expiry after Connect so the expiry timer, rather than a request, observes it.
			if scenario == "expiry" {
				r.tokens.mu.Lock()
				r.tokens.saved.Expiry = time.Now().Add(100 * time.Millisecond)
				r.tokens.mu.Unlock()
			}
			owner := hubClient(t, hub, "old")
			done := make(chan error, 1)
			go func() { done <- m.Start(t.Context()) }()
			if scenario == "401" || scenario == "401-no-refresh" {
				f.runtimeStatus.Store(http.StatusUnauthorized)
			}
			if scenario == "step-up" {
				f.runtimeStatus.Store(http.StatusForbidden)
			}
			if scenario != "expiry" && scenario != "expired-startup" {
				result, err := owner.CallTool(t.Context(), &mcp.CallToolParams{Name: "wiki_echo", Arguments: map[string]any{}})
				require.NoError(t, err)
				require.True(t, result.IsError)
				assert.Contains(t, result.Content[0].(*mcp.TextContent).Text, "run mcp-login wiki")
			}
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("auth failure did not disable remote")
			}
			require.NoError(t, m.Shutdown(t.Context()))
			items, err := inbox.Active(t.Context())
			require.NoError(t, err)
			require.Len(t, items, 1)
			assert.Contains(t, items[0].Payload.Notice, "run mcp-login wiki")
			assert.Equal(t, "owner-notice", items[0].Payload.Context.TaskName)
			if scenario == "step-up" {
				assert.Contains(t, items[0].Payload.Notice, `check mcp_servers.scopes for "write"`)
			}
			assert.NotContains(t, items[0].Payload.Notice, "secret-value")
			newSession := hubClient(t, hub, "new")
			listed, err := newSession.ListTools(t.Context(), nil)
			require.NoError(t, err)
			assert.Empty(t, listed.Tools)
			if scenario != "expired-startup" {
				_, err := owner.CallTool(t.Context(), &mcp.CallToolParams{Name: "wiki_echo", Arguments: map[string]any{}})
				require.ErrorContains(t, err, "unknown tool")
			}
			if scenario == "401" {
				claimed, err := inbox.Claim(t.Context(), items[0].ID, time.Now().Add(time.Minute))
				require.NoError(t, err)
				require.True(t, claimed)
				_, err = inbox.Finish(t.Context(), items[0].ID, "", 0, time.Time{})
				require.NoError(t, err)
				stored, err = repo.Get(t.Context(), config.Name)
				require.NoError(t, err)
				stored.Expiry = time.Now().Add(-time.Minute)
				require.NoError(t, repo.Save(t.Context(), config.Name, stored))
				f.runtimeStatus.Store(http.StatusUnauthorized)
				restartedHub, err := toolhub.New(toolhub.Config{})
				require.NoError(t, err)
				defer restartedHub.Shutdown(t.Context())
				restarted, err := New([]ServerConfig{config}, repo, inbox, restartedHub, slog.New(slog.DiscardHandler))
				require.NoError(t, err)
				require.NoError(t, restarted.Connect(t.Context()))
				defer restarted.Shutdown(t.Context())
				var count int
				require.NoError(t, db.QueryRow("SELECT count(*) FROM inbox").Scan(&count))
				assert.Equal(t, 1, count)
			}
			assert.EqualValues(t, 1, f.registrations.Load(), "runtime must never register")
		})
	}
}

func TestForbiddenWithoutScopeChallengeDoesNotInvalidate(t *testing.T) {
	const endpoint = "https://remote.example/mcp"
	token := &oauth2.Token{AccessToken: "access"}
	tokens := &tokenSource{ctx: t.Context(), saved: token, source: oauth2.StaticTokenSource(token)}
	transport := authorizedTransport(endpoint, tokens, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(bytes.NewBufferString("private details"))}, nil
	}))
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, nil)
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	response.Body.Close()
	assert.Equal(t, 403, response.StatusCode)
	assert.NoError(t, tokens.err())
	_, err = tokens.Token()
	require.NoError(t, err)
}

type testTokenSource func() (*oauth2.Token, error)

func (f testTokenSource) Token() (*oauth2.Token, error) { return f() }

func TestRefreshErrorsOnlyInvalidateRejectedGrants(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
	}{
		{"DNS", &net.DNSError{Err: "secret-value", IsTemporary: true}, false},
		{"500", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 500}}, false},
		{"429", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 429}}, false},
		{"408", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 408}}, false},
		{"unavailable", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 400}, ErrorCode: "temporarily_unavailable"}, false},
		{"invalid_grant", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 400}, ErrorCode: "invalid_grant", Body: []byte("secret-value")}, true},
		{"401", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 401}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			valid := &oauth2.Token{AccessToken: "access", RefreshToken: "refresh"}
			calls := 0
			source := &tokenSource{ctx: t.Context(), server: "test", saved: valid, failed: make(chan struct{}), source: testTokenSource(func() (*oauth2.Token, error) {
				calls++
				if calls == 1 {
					return nil, tc.err
				}
				return valid, nil
			})}
			_, err := source.Token()
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "secret-value")
			assert.Equal(t, tc.permanent, source.err() != nil)
			_, err = source.Token()
			if tc.permanent {
				require.ErrorContains(t, err, "run mcp-login")
				assert.Equal(t, 1, calls)
			} else {
				require.NoError(t, err)
				assert.Equal(t, 2, calls)
			}
		})
	}
}

func TestRuntime401RefreshAndRetry(t *testing.T) {
	f := newOAuthFixture(t)
	repo := loginRepo(t)
	config := ServerConfig{Name: "wiki", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}
	require.NoError(t, Login(t.Context(), config, repo, browser(t), nil))
	tokens, err := restoreTokens(t.Context(), config, repo)
	require.NoError(t, err)
	hub, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	defer hub.Shutdown(t.Context())
	require.NoError(t, hub.AddSource("external:wiki", nil))
	r := &remote{config: config, tokens: tokens, hub: hub, client: mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil), logger: slog.New(slog.DiscardHandler)}
	require.NoError(t, r.connect(t.Context()))
	defer r.close()
	f.mu.Lock()
	f.access = "revoked-access"
	f.mu.Unlock()
	f.refreshStatus.Store(503)
	_, err = r.session.ListTools(t.Context(), nil)
	require.ErrorContains(t, err, "retry later")
	require.NoError(t, tokens.err())
	f.refreshStatus.Store(0)
	_, err = r.session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, tokens.err())
	assert.EqualValues(t, 1, f.refreshes.Load())
	stored, err := repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	assert.Equal(t, "rotated-refresh", stored.RefreshToken)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { assert.NoError(t, tokens.refreshRejected("Bearer initial-access")) })
	}
	wg.Wait()
	assert.EqualValues(t, 1, f.refreshes.Load(), "stale concurrent 401s reuse the refreshed token")
	for _, status := range []int{503, 0} {
		f.mu.Lock()
		f.access = "revoked-again"
		f.mu.Unlock()
		f.runtimeStatus.Store(int32(status))
		_, err := r.session.ListTools(t.Context(), nil)
		if status == 0 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
		require.NoError(t, tokens.err())
	}
	assert.EqualValues(t, 3, f.refreshes.Load(), "a transient retry response must not poison the next request")

}

func TestRuntimeRedirectDoesNotForwardToken(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer access", r.Header.Get("Authorization"))
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	token := &oauth2.Token{AccessToken: "access"}
	source := &tokenSource{ctx: t.Context(), saved: token, source: oauth2.StaticTokenSource(token)}
	client := &http.Client{Transport: authorizedTransport(origin.URL, source, http.DefaultTransport)}
	resp, err := client.Get(origin.URL)
	if resp != nil {
		resp.Body.Close()
	}
	require.ErrorContains(t, err, "cross-origin")
	assert.False(t, called)
}

func TestExpiryWarningOnlyWithinThirtyDays(t *testing.T) {
	for _, days := range []int{0, 30, 31} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			f := newOAuthFixture(t)
			f.refresh = ""
			repo := loginRepo(t)
			config := ServerConfig{Name: "wiki", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}
			require.NoError(t, Login(t.Context(), config, repo, browser(t), nil))
			stored, err := repo.Get(t.Context(), config.Name)
			require.NoError(t, err)
			stored.Expiry = time.Now().Add(time.Duration(days) * 24 * time.Hour)
			if days == 0 {
				stored.Expiry = time.Time{}
			}
			require.NoError(t, repo.Save(t.Context(), config.Name, stored))
			hub, err := toolhub.New(toolhub.Config{})
			require.NoError(t, err)
			defer hub.Shutdown(t.Context())
			var logs bytes.Buffer
			m, err := New([]ServerConfig{config}, repo, &discardNoticeInbox{}, hub, slog.New(slog.NewTextHandler(&logs, nil)))
			require.NoError(t, err)
			require.NoError(t, m.Connect(t.Context()))
			defer m.Shutdown(t.Context())
			assert.Equal(t, days != 0 && days <= 30, strings.Contains(logs.String(), "MCP login expires without a refresh token"))
		})
	}
}

type discardNoticeInbox struct{ domain.InboxRepository }

func (*discardNoticeInbox) Enqueue(context.Context, domain.InboxItem) (int64, error) { return 1, nil }
