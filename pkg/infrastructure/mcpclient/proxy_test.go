package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func hubClient(t *testing.T, hub domain.ToolHub, key string, actor domain.Actor) *mcp.ClientSession {
	t.Helper()
	endpoint, err := hub.OpenSession(domain.SessionKey(key), actor)
	require.NoError(t, err)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	httpClient := oauth2.NewClient(t.Context(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: endpoint.BearerToken, TokenType: "Bearer"}))
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: endpoint.URL, HTTPClient: httpClient}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close(); hub.CloseSession(domain.SessionKey(key)) })
	return session
}

func TestProxySharedPermissionsAndReconnect(t *testing.T) {
	f := newOAuthFixture(t)
	var calls atomic.Int32
	add := func(name string) {
		f.mcp.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls.Add(1)
			var args struct {
				Text string `json:"text"`
			}
			require.NoError(t, json.Unmarshal(request.Params.Arguments, &args))
			return &mcp.CallToolResult{IsError: args.Text == "fail", Content: []mcp.Content{&mcp.TextContent{Text: args.Text}, &mcp.TextContent{Text: "tail"}}}, nil
		})
	}
	add("echo")
	add("excluded")
	add("future")
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "oauth.db")})
	require.NoError(t, err)
	defer db.Close()
	repo := repository.NewOAuthTokenRepository(db)
	config := ServerConfig{Name: "tracker", URL: f.server.URL + "/mcp", Scopes: []string{"read"}, Tools: []string{"echo", "new"}}
	require.NoError(t, Login(t.Context(), config, repo, browser(t)))
	hub, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	hubDone := make(chan error, 1)
	go func() { hubDone <- hub.Start(context.Background()) }()
	defer func() { require.NoError(t, hub.Shutdown(t.Context())); require.NoError(t, <-hubDone) }()
	m, err := New([]ServerConfig{config}, repo, repository.NewInboxRepository(db), hub, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, m.Connect(ctx))
	initial := m.(*manager).remotes[0].session
	done := make(chan error, 1)
	go func() { done <- m.Start(ctx) }()
	defer func() { cancel(); require.NoError(t, <-done) }()
	owner := hubClient(t, hub, "owner", domain.ActorOwner)
	member := hubClient(t, hub, "member", domain.ActorMember)
	second := hubClient(t, hub, "second", domain.ActorOwner)
	listed, err := owner.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, 1)
	assert.Equal(t, "tracker_echo", listed.Tools[0].Name)
	call := func(session *mcp.ClientSession, text string) *mcp.CallToolResult {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "tracker_echo", Arguments: map[string]any{"text": text}})
		require.NoError(t, err)
		return result
	}
	assert.True(t, call(member, "denied").IsError)
	assert.Zero(t, calls.Load())
	result := call(owner, "hello")
	require.False(t, result.IsError)
	assert.Equal(t, "hello\ntail", result.Content[0].(*mcp.TextContent).Text)
	assert.True(t, call(owner, "fail").IsError)
	require.NoError(t, owner.Close())
	hub.CloseSession("owner")
	assert.False(t, call(second, "still connected").IsError)
	assert.EqualValues(t, 2, f.initializations.Load())
	assert.EqualValues(t, 3, calls.Load())
	f.mcp.RemoveTools("echo")
	add("new")
	require.NoError(t, initial.Close())
	require.Eventually(t, func() bool {
		listed, err := second.ListTools(t.Context(), nil)
		return err == nil && len(listed.Tools) == 1 && listed.Tools[0].Name == "tracker_new" && f.initializations.Load() == 3
	}, 2*time.Second, 10*time.Millisecond)
	result, err = second.CallTool(t.Context(), &mcp.CallToolParams{Name: "tracker_new", Arguments: map[string]any{"text": "reconnected"}})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.EqualValues(t, 1, f.registrations.Load())
}

func TestMissingLoginSkipsAndDefaults(t *testing.T) {
	f := newOAuthFixture(t)
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "oauth.db")})
	require.NoError(t, err)
	defer db.Close()
	hub, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	defer hub.Shutdown(t.Context())
	var logs bytes.Buffer
	m, err := New([]ServerConfig{{Name: "missing", URL: f.server.URL + "/mcp"}}, repository.NewOAuthTokenRepository(db), repository.NewInboxRepository(db), hub, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, m.Connect(ctx))
	assert.Contains(t, logs.String(), "noctiluca mcp-login missing")
	assert.Contains(t, logs.String(), "level=WARN")
	assert.Zero(t, f.initializations.Load())
	cancel()
	require.NoError(t, m.Start(ctx))
}

type failingTokens struct{ domain.OAuthTokenRepository }

func (f failingTokens) Save(context.Context, string, domain.OAuthToken) error {
	return errors.New("private database error")
}

func TestRefreshFailureAndPersistenceRetry(t *testing.T) {
	f := newOAuthFixture(t)
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "oauth.db")})
	require.NoError(t, err)
	defer db.Close()
	repo := repository.NewOAuthTokenRepository(db)
	config := ServerConfig{Name: "tracker", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}
	require.NoError(t, Login(t.Context(), config, repo, browser(t)))
	stored, err := repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	stored.Expiry = time.Now().Add(-time.Minute)
	require.NoError(t, repo.Save(t.Context(), config.Name, stored))
	source, err := restoreTokens(t.Context(), config, failingTokens{repo})
	require.NoError(t, err)
	_, err = source.Token()
	require.ErrorContains(t, err, "persist MCP tokens")
	source.repository = repo
	err = source.refreshRejected("Bearer initial-access")
	require.NoError(t, err)
	assert.EqualValues(t, 1, f.refreshes.Load())
	stored, err = repo.Get(t.Context(), config.Name)
	require.NoError(t, err)
	assert.Equal(t, "rotated-refresh", stored.RefreshToken)
	stored.Expiry = time.Now().Add(-time.Minute)
	require.NoError(t, repo.Save(t.Context(), config.Name, stored))
	f.mu.Lock()
	f.denyRefresh = true
	f.mu.Unlock()
	source, err = restoreTokens(t.Context(), config, repo)
	require.NoError(t, err)
	_, err = source.Token()
	require.ErrorContains(t, err, "mcp-login")
	assert.NotContains(t, err.Error(), "secret-value")
}

func TestShutdownConnectedBeforeStart(t *testing.T) {
	f := newOAuthFixture(t)
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "oauth.db")})
	require.NoError(t, err)
	defer db.Close()
	repo := repository.NewOAuthTokenRepository(db)
	config := ServerConfig{Name: "tracker", URL: f.server.URL + "/mcp", Scopes: []string{"read"}}
	require.NoError(t, Login(t.Context(), config, repo, browser(t)))
	hub, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	defer hub.Shutdown(t.Context())
	m, err := New([]ServerConfig{config}, repo, repository.NewInboxRepository(db), hub, nil)
	require.NoError(t, err)
	require.NoError(t, m.Connect(t.Context()))
	session := m.(*manager).remotes[0].session
	require.NotNil(t, session)
	wait := make(chan error, 1)
	go func() { wait <- session.Wait() }()
	require.NoError(t, m.Shutdown(t.Context()))
	select {
	case err := <-wait:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("connected session did not close")
	}
	require.NoError(t, m.Shutdown(t.Context()))
	require.Error(t, m.Start(t.Context()))
	require.Error(t, m.Connect(t.Context()))
}

type blockedTokens struct {
	domain.OAuthTokenRepository
	entered chan struct{}
}

func (b blockedTokens) Get(ctx context.Context, _ string) (domain.OAuthToken, error) {
	close(b.entered)
	<-ctx.Done()
	return domain.OAuthToken{}, ctx.Err()
}

func TestShutdownDuringConnect(t *testing.T) {
	hub, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	defer hub.Shutdown(t.Context())
	entered := make(chan struct{})
	m, err := New([]ServerConfig{{Name: "tracker", URL: "http://127.0.0.1:1/mcp"}}, blockedTokens{entered: entered}, &unusedInbox{}, hub, nil)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- m.Connect(t.Context()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Connect did not reach repository")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, m.Shutdown(ctx))
	require.ErrorIs(t, <-done, context.Canceled)
	require.Error(t, m.Start(t.Context()))
}

func TestProxyMemberOptInSkipsInvalidNames(t *testing.T) {
	f := newOAuthFixture(t)
	long := strings.Repeat("a", 124)
	for _, name := range []string{"echo", long} {
		f.mcp.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	}
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "db")})
	require.NoError(t, err)
	defer db.Close()
	config := ServerConfig{Name: "tracker", URL: f.server.URL + "/mcp", Scopes: []string{"read"}, Members: true}
	repo := repository.NewOAuthTokenRepository(db)
	require.NoError(t, Login(t.Context(), config, repo, browser(t)))
	hub, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- hub.Start(t.Context()) }()
	defer func() { require.NoError(t, hub.Shutdown(t.Context())); require.NoError(t, <-done) }()
	var logs bytes.Buffer
	manager, err := New([]ServerConfig{config}, repo, repository.NewInboxRepository(db), hub, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	require.NoError(t, manager.Connect(t.Context()))
	defer manager.Shutdown(t.Context())
	require.Contains(t, logs.String(), "tracker_"+long)
	member := hubClient(t, hub, "member", domain.ActorMember)
	listed, err := member.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, 1)
	require.Equal(t, "tracker_echo", listed.Tools[0].Name)
	result, err := member.CallTool(t.Context(), &mcp.CallToolParams{Name: "tracker_echo", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError)
}

// Connect is cancelled in the repository before it can enqueue a notice.
type unusedInbox struct{ domain.InboxRepository }
