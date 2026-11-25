package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/usecase/toolhub"
)

func elicitationProxyClient(t *testing.T, server *mcp.Server, stateless bool) *mcp.ClientSession {
	t.Helper()
	const access = "test-access"
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: stateless, JSONResponse: true})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+access {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		transport.ServeHTTP(w, r)
	}))
	t.Cleanup(remote.Close)
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "oauth.db")})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	repo := repository.NewOAuthTokenRepository(db)
	config := ServerConfig{Name: "workspace", URL: remote.URL + "/mcp"}
	reg := registration{Resource: config.URL, OAuth: oauth2.Config{ClientID: "test-client", Endpoint: oauth2.Endpoint{TokenURL: remote.URL + "/token"}}}
	require.NoError(t, saveToken(t.Context(), repo, config.Name, reg, &oauth2.Token{AccessToken: access, TokenType: "Bearer"}))

	hub, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	hubDone := make(chan error, 1)
	go func() { hubDone <- hub.Start(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, hub.Shutdown(ctx))
		assert.NoError(t, <-hubDone)
	})
	manager, err := New([]ServerConfig{config}, repo, repository.NewInboxRepository(db), hub, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, manager.Shutdown(ctx))
	})
	require.NoError(t, manager.Connect(t.Context()))
	return hubClient(t, hub, "owner")
}

func elicitationCall(t *testing.T, client *mcp.ClientSession, name string, args string, isError bool) string {
	t.Helper()
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
	require.NoError(t, err)
	require.Equal(t, isError, result.IsError)
	require.Len(t, result.Content, 1)
	content, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return content.Text
}

func TestProxyFormElicitation(t *testing.T) {
	const state = "opaque-approval-state"
	const id = "approval_choice"
	const message = "Send the message?"
	const capabilities = `{"elicitation":{"form":{}}}`
	forms := mcp.InputRequestMap{id: &mcp.ElicitParams{Mode: "form", Message: message, RequestedSchema: json.RawMessage(`{
		"type":"object","properties":{"choice":{"type":"string","enum":["yes","no"],"enumNames":["Send it","Do not send"]}},"required":["choice"]
	}`)}}
	server := mcp.NewServer(&mcp.Implementation{Name: "workspace", Version: "1"}, nil)
	discoveries := make(chan mcp.Meta, 2)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if discover, ok := request.GetParams().(*mcp.DiscoverParams); ok {
				discoveries <- discover.Meta
			}
			return next(ctx, method, request)
		}
	})
	requests := make(chan *mcp.CallToolRequest, 2)
	schema := json.RawMessage(`{"type":"object","properties":{"z":{"type":"object"},"a":{"type":"string"}},"required":["z","a"],"additionalProperties":false}`)
	server.AddTool(&mcp.Tool{Name: "send", InputSchema: schema}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		select {
		case requests <- request:
		default:
		}
		if request.Params.RequestState == "" {
			return &mcp.CallToolResult{InputRequests: forms, RequestState: state}, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Sent"}}}, nil
	})
	client := elicitationProxyClient(t, server, true)
	listed, err := client.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, 1)
	require.Equal(t, "workspace_send", listed.Tools[0].Name)
	assert.Contains(t, listed.Tools[0].InputSchema.(map[string]any)["properties"], "_mcp_retry")
	assert.Equal(t, false, listed.Tools[0].InputSchema.(map[string]any)["additionalProperties"])
	require.Len(t, discoveries, 1)
	discovery := <-discoveries
	assert.Equal(t, "2026-07-28", discovery[mcp.MetaKeyProtocolVersion])
	advertised, err := json.Marshal(discovery[mcp.MetaKeyClientCapabilities])
	require.NoError(t, err)
	assert.JSONEq(t, capabilities, string(advertised))

	const args = `{"z":{"integer":9007199254740993},"a":"<>&"}`
	text := elicitationCall(t, client, "workspace_send", args, false)
	require.Len(t, requests, 1)
	first := <-requests
	assert.Equal(t, "2026-07-28", first.Params.Meta[mcp.MetaKeyProtocolVersion])
	advertised, err = json.Marshal(first.Params.Meta[mcp.MetaKeyClientCapabilities])
	require.NoError(t, err)
	assert.JSONEq(t, capabilities, string(advertised))
	assert.Empty(t, first.Params.RequestState)
	assert.Empty(t, first.Params.InputResponses)
	instructions, challenge, found := strings.Cut(text, "\n{")
	require.True(t, found)
	for _, phrase := range []string{"workspace_send did not run", "Ask the owner in chat", "but not the requestState", "Do not wait", "Never choose for the owner", `call workspace_send again with the same arguments plus "_mcp_retry"`, "denies the call, or drops the request, do not call workspace_send again", `call workspace_send without "_mcp_retry" to get a new request`} {
		assert.Contains(t, instructions, phrase)
	}
	formJSON, err := json.Marshal(forms)
	require.NoError(t, err)
	assert.JSONEq(t, `{"inputRequests":`+string(formJSON)+`,"requestState":"`+state+`"}`, "{"+challenge)

	retry := strings.TrimSuffix(args, "}") + `,"_mcp_retry":{"requestState":"` + state + `","inputResponses":{"` + id + `":{"action":"accept","content":{"choice":"yes"}}}}}`
	assert.Equal(t, "Sent", elicitationCall(t, client, "workspace_send", retry, false))
	require.Len(t, requests, 1)
	second := <-requests
	assert.Equal(t, state, second.Params.RequestState)
	assert.Equal(t, mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "accept", Content: map[string]any{"choice": "yes"}}}, second.Params.InputResponses)
	for _, request := range []*mcp.CallToolRequest{first, second} {
		var values map[string]any
		decoder := json.NewDecoder(bytes.NewReader(request.Params.Arguments))
		decoder.UseNumber()
		require.NoError(t, decoder.Decode(&values))
		assert.Equal(t, map[string]any{"z": map[string]any{"integer": json.Number("9007199254740993")}, "a": "<>&"}, values)
	}
	for _, tc := range []struct{ name, retry string }{
		{"missing_responses", `{}`},
		{"empty_responses", `{"inputResponses":{}}`},
		{"missing_action", `{"inputResponses":{"approval_choice":{"content":{"choice":"yes"}}}}`},
		{"roots_answer", `{"inputResponses":{"approval_choice":{"roots":[]}}}`},
		{"invalid_state_type", `{"requestState":1,"inputResponses":{"approval_choice":{"action":"accept"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := strings.TrimSuffix(args, "}") + `,"_mcp_retry":` + tc.retry + `}`
			assert.Contains(t, elicitationCall(t, client, "workspace_send", invalid, true), "invalid _mcp_retry")
			assert.Empty(t, requests)
		})
	}
}

func TestProxyUnsupportedElicitation(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "workspace", Version: "1"}, nil)
	tests := []struct {
		name, schema, args, reason string
		requests                   mcp.InputRequestMap
		serverOwnedRetry           bool
	}{
		{"busy", `{"type":"object","properties":null}`, `{}`, "server is busy", mcp.InputRequestMap{}, false},
		{"url", `{"type":"object"}`, `{}`, "cannot pass on", mcp.InputRequestMap{"url": &mcp.ElicitParams{Mode: "url", URL: "https://example.com/approval"}}, false},
		{"reserved", `{"type":"object","properties":{"_mcp_retry":{"type":"string"}},"required":["_mcp_retry"],"additionalProperties":false}`, `{"_mcp_retry":"server value"}`, "cannot pass on", mcp.InputRequestMap{"form": &mcp.ElicitParams{Message: "Confirm"}}, true},
	}
	for _, tc := range tests {
		server.AddTool(&mcp.Tool{Name: tc.name, InputSchema: json.RawMessage(tc.schema)}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			assert.JSONEq(t, tc.args, string(request.Params.Arguments))
			assert.Empty(t, request.Params.InputResponses)
			return &mcp.CallToolResult{InputRequests: tc.requests, RequestState: "opaque"}, nil
		})
	}
	client := elicitationProxyClient(t, server, true)
	listed, err := client.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, len(tests))
	listedSchemas := make(map[string]any)
	for _, tool := range listed.Tools {
		listedSchemas[tool.Name] = tool.InputSchema
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := "workspace_" + tc.name
			require.Contains(t, listedSchemas, name)
			if tc.serverOwnedRetry {
				schema, err := json.Marshal(listedSchemas[name])
				require.NoError(t, err)
				assert.JSONEq(t, tc.schema, string(schema))
			} else {
				assert.Contains(t, listedSchemas[name].(map[string]any)["properties"], "_mcp_retry")
			}
			text := elicitationCall(t, client, name, tc.args, true)
			assert.Contains(t, text, "did not run")
			assert.Contains(t, text, tc.reason)
		})
	}
}

func TestProxyLegacyElicitationCapability(t *testing.T) {
	const args = `{"z":"last","a":"first"}`
	server := mcp.NewServer(&mcp.Implementation{Name: "workspace", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "echo", InputSchema: json.RawMessage(`{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"string"}},"additionalProperties":false}`)}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		init := request.Session.InitializeParams()
		if assert.NotNil(t, init) {
			assert.Equal(t, "2025-11-25", init.ProtocolVersion)
			if assert.NotNil(t, init.Capabilities) {
				assert.Nil(t, init.Capabilities.Elicitation)
			}
		}
		assert.Equal(t, args, string(request.Params.Arguments))
		assert.Empty(t, request.Params.RequestState)
		assert.Empty(t, request.Params.InputResponses)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Echoed"}}}, nil
	})
	client := elicitationProxyClient(t, server, false)
	listed, err := client.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, 1)
	assert.NotContains(t, listed.Tools[0].InputSchema.(map[string]any)["properties"], "_mcp_retry")
	assert.Equal(t, "Echoed", elicitationCall(t, client, "workspace_echo", args, false))
}
