package toolhub_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/usecase/toolhub"
	"github.com/stretchr/testify/require"
)

func startHub(t *testing.T) domain.ToolHub {
	t.Helper()
	h, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- h.Start(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, h.Shutdown(ctx))
		require.NoError(t, <-done)
	})
	return h
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func client(t *testing.T, endpoint domain.MCPEndpoint) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	transport := &http.Transport{}
	s, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: endpoint.URL, HTTPClient: &http.Client{Transport: bearerTransport{endpoint.BearerToken, transport}, Timeout: 3 * time.Second}}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(); transport.CloseIdleConnections() })
	require.Equal(t, "noctiluca", s.InitializeResult().ServerInfo.Name)
	return s
}

func echo(name string) domain.Tool {
	return domain.Tool{Name: name, Description: "Echo test input", InputSchema: json.RawMessage(`{"type":"object"}`), Handler: func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil }}
}

func call(t *testing.T, c *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	result, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

func TestSourcesContextAndPermissions(t *testing.T) {
	h := startHub(t)
	identity := echo("identity")
	identity.Handler = func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		session, ok := domain.ToolSessionFromContext(ctx)
		if !ok {
			return nil, errors.New("missing session identity")
		}
		return json.Marshal(session)
	}
	owner := echo("owner_echo")
	owner.OwnerOnly = true
	require.NoError(t, h.AddSource("core", []domain.Tool{identity}))
	require.NoError(t, h.AddSource("adapter", []domain.Tool{owner}))
	memberEndpoint, err := h.OpenSession("member-session", domain.ActorMember)
	require.NoError(t, err)
	ownerEndpoint, err := h.OpenSession("owner-session", domain.ActorOwner)
	require.NoError(t, err)
	require.NotEqual(t, ownerEndpoint.BearerToken, memberEndpoint.BearerToken)
	memberClient := client(t, memberEndpoint)
	ownerClient := client(t, ownerEndpoint)
	require.NoError(t, h.AddSource("external", []domain.Tool{echo("external_echo")}))
	listed, err := memberClient.ListTools(t.Context(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	require.ElementsMatch(t, []string{"identity", "owner_echo", "external_echo"}, names)
	require.True(t, call(t, memberClient, "owner_echo", map[string]any{}).IsError)
	require.False(t, call(t, ownerClient, "owner_echo", map[string]any{}).IsError)
	for _, tc := range []struct {
		c     *mcp.ClientSession
		key   string
		actor domain.Actor
	}{{memberClient, "member-session", domain.ActorMember}, {ownerClient, "owner-session", domain.ActorOwner}} {
		result := call(t, tc.c, "identity", map[string]any{"actor": "owner", "key": "forged"})
		require.False(t, result.IsError)
		require.Len(t, result.Content, 1)
		require.JSONEq(t, fmt.Sprintf(`{"Key":%q,"Actor":%q}`, tc.key, tc.actor), result.Content[0].(*mcp.TextContent).Text)
		require.Nil(t, result.StructuredContent)
	}
	raw := echo("raw")
	raw.Handler = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage("  {\"ok\": true}\n"), nil
	}
	require.NoError(t, h.AddSource("raw", []domain.Tool{raw}))
	require.Equal(t, "  {\"ok\": true}\n", call(t, ownerClient, "raw", nil).Content[0].(*mcp.TextContent).Text)
}

func TestSessionLifetime(t *testing.T) {
	h := startHub(t)
	require.NoError(t, h.AddSource("core", []domain.Tool{echo("echo")}))
	endpoint, err := h.OpenSession("session", domain.ActorMember)
	require.NoError(t, err)
	first := client(t, endpoint)
	require.False(t, call(t, first, "echo", map[string]any{"turn": 1}).IsError)
	require.NoError(t, first.Close())
	// A subagent can open a new transport after the parent turn and its transport end.
	background := client(t, endpoint)
	require.False(t, call(t, background, "echo", map[string]any{"background": true}).IsError)
	_, err = h.OpenSession("session", domain.ActorOwner)
	require.Error(t, err)
	h.CloseSession("session")
	h.CloseSession("session")
	_, err = background.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo"})
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, requestStatus(t, endpoint, "Bearer "+endpoint.BearerToken))
	replacement, err := h.OpenSession("session", domain.ActorOwner)
	require.NoError(t, err)
	require.NotEqual(t, endpoint.BearerToken, replacement.BearerToken)
	require.False(t, call(t, client(t, replacement), "echo", nil).IsError)
	require.Equal(t, http.StatusUnauthorized, requestStatus(t, endpoint, "Bearer "+endpoint.BearerToken))
}

func requestStatus(t *testing.T, endpoint domain.MCPEndpoint, authorization string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", authorization)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NotContains(t, string(body), endpoint.BearerToken)
	return resp.StatusCode
}

func TestInvalidTokensAndRegistry(t *testing.T) {
	h := startHub(t)
	endpoint, err := h.OpenSession("session", domain.ActorMember)
	require.NoError(t, err)
	for _, auth := range []string{"", "Bearer wrong", "Basic " + endpoint.BearerToken, "Bearer " + endpoint.BearerToken + " extra"} {
		require.Equal(t, http.StatusUnauthorized, requestStatus(t, endpoint, auth))
	}
	require.NoError(t, h.AddSource("core", []domain.Tool{echo("echo")}))
	require.Error(t, h.AddSource("core", []domain.Tool{echo("other")}))
	require.Error(t, h.AddSource("collision", []domain.Tool{echo("new"), echo("echo")}))
	require.Error(t, h.AddSource("duplicate", []domain.Tool{echo("dup"), echo("dup")}))
	require.Error(t, h.AddSource("", nil))
	for _, schema := range []string{"null", `{}`, `{"type":"string"}`, `{"type":"object","$ref":"#/$defs/missing"}`} {
		invalid := echo("invalid")
		invalid.InputSchema = json.RawMessage(schema)
		require.Error(t, h.AddSource("invalid", []domain.Tool{invalid}))
	}
	invalid := echo("no handler")
	require.Error(t, h.AddSource("invalid", []domain.Tool{invalid}))
	invalid = echo("nil_handler")
	invalid.Handler = nil
	require.Error(t, h.AddSource("invalid", []domain.Tool{invalid}))
	c := client(t, endpoint)
	list, err := c.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, list.Tools, 1)
	strict := echo("strict")
	strict.InputSchema = json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}},"additionalProperties":false}`)
	require.NoError(t, h.AddSource("strict", []domain.Tool{strict}))
	require.True(t, call(t, c, "strict", map[string]any{"text": 5}).IsError)
	require.True(t, call(t, c, "strict", nil).IsError)
	require.False(t, call(t, c, "strict", map[string]any{"text": "ok"}).IsError)
	failed := echo("failed")
	failed.Handler = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("test failure")
	}
	badJSON := echo("bad_json")
	badJSON.Handler = func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage("broken"), nil }
	require.NoError(t, h.AddSource("errors", []domain.Tool{failed, badJSON}))
	require.Equal(t, "test failure", call(t, c, "failed", nil).Content[0].(*mcp.TextContent).Text)
	require.True(t, call(t, c, "bad_json", nil).IsError)
	_, err = h.OpenSession("", domain.ActorOwner)
	require.Error(t, err)
	_, err = h.OpenSession("invalid", domain.Actor("invalid"))
	require.Error(t, err)
}

func TestCloseCancelsActiveCall(t *testing.T) {
	h := startHub(t)
	entered := make(chan struct{})
	canceled := make(chan struct{})
	waiting := echo("wait")
	waiting.Handler = func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}
	require.NoError(t, h.AddSource("core", []domain.Tool{waiting}))
	endpoint, err := h.OpenSession("session", domain.ActorMember)
	require.NoError(t, err)
	c := client(t, endpoint)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = c.CallTool(t.Context(), &mcp.CallToolParams{Name: "wait"}) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	h.CloseSession("session")
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not canceled")
	}
	<-done
}

func TestPortsAndShutdown(t *testing.T) {
	h := startHub(t)
	ep, err := h.OpenSession("session", domain.ActorMember)
	require.NoError(t, err)
	parsed, err := url.Parse(ep.URL)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", parsed.Hostname())
	require.NotEqual(t, "0", parsed.Port())
	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)
	_, err = toolhub.New(toolhub.Config{Port: port})
	require.Error(t, err)
	require.NoError(t, h.Shutdown(t.Context()))
	require.NoError(t, h.Shutdown(t.Context()))
	_, err = h.OpenSession("closed", domain.ActorMember)
	require.Error(t, err)
	require.Error(t, h.AddSource("closed", nil))
	replacement, err := toolhub.New(toolhub.Config{Port: port})
	require.NoError(t, err)
	require.NoError(t, replacement.Shutdown(t.Context()))
	listener, err := net.Listen("tcp4", parsed.Host)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	for _, port := range []int{-1, 65536} {
		_, err := toolhub.New(toolhub.Config{Port: port})
		require.Error(t, err)
	}
}

func TestConcurrentSessions(t *testing.T) {
	h := startHub(t)
	require.NoError(t, h.AddSource("core", []domain.Tool{echo("echo")}))
	var wg sync.WaitGroup
	for i := range 12 {
		key := domain.SessionKey(fmt.Sprintf("session-%d", i))
		wg.Go(func() {
			ep, err := h.OpenSession(key, domain.ActorMember)
			if err != nil {
				t.Error(err)
				return
			}
			c := client(t, ep)
			call(t, c, "echo", map[string]any{"session": key})
			h.CloseSession(key)
		})
	}
	wg.Wait()
}

func TestStartCancellationDrainsHandlers(t *testing.T) {
	h, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- h.Start(ctx) }()
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }); _ = h.Shutdown(context.Background()) })
	tool := echo("cleanup")
	tool.Handler = func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return json.RawMessage(`{}`), nil
	}
	require.NoError(t, h.AddSource("core", []domain.Tool{tool}))
	ep, err := h.OpenSession("session", domain.ActorMember)
	require.NoError(t, err)
	c := client(t, ep)
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = c.CallTool(context.Background(), &mcp.CallToolParams{Name: "cleanup"})
	}()
	<-entered
	cancel()
	<-canceled
	select {
	case err := <-started:
		t.Fatalf("Start returned before cleanup: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	require.NoError(t, <-started)
	<-callDone
}
