package toolhub_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/usecase/toolhub"
	"github.com/stretchr/testify/require"
)

func TestAlwaysOfferedToolMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]any
	}{
		{name: "configured", meta: map[string]any{"test/alwaysOffered": true}},
		{name: "unconfigured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHub(t, toolhub.Config{AlwaysOfferedMeta: tc.meta})
			onDemand := echo("on_demand")
			onDemand.OnDemand = true
			require.NoError(t, h.AddSource("tools", []domain.Tool{echo("built_in"), onDemand}))
			endpoint, err := h.OpenSession("session")
			require.NoError(t, err)
			c := client(t, endpoint)
			listed, err := c.ListTools(t.Context(), nil)
			require.NoError(t, err)
			require.Len(t, listed.Tools, 2)
			for _, tool := range listed.Tools {
				if tool.Name == "built_in" {
					require.Equal(t, mcp.Meta(tc.meta), tool.Meta)
				} else {
					require.Equal(t, "on_demand", tool.Name)
					require.Nil(t, tool.Meta)
				}
			}
		})
	}
}

func TestReplaceSourceAndTextResults(t *testing.T) {
	h := startHub(t, toolhub.Config{})
	require.NoError(t, h.AddSource("core", []domain.Tool{echo("reserved")}))
	require.NoError(t, h.AddSource("remote", []domain.Tool{echo("old"), echo("kept")}))
	endpoint, err := h.OpenSession("member")
	require.NoError(t, err)
	c := client(t, endpoint)
	require.Error(t, h.ReplaceSource("remote", []domain.Tool{echo("reserved")}))
	require.False(t, call(t, c, "old", nil).IsError)
	kept := echo("kept")
	text := echo("text")
	text.TextResult = true
	text.Handler = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`  "unwrapped"`), nil
	}
	require.NoError(t, h.ReplaceSource("remote", []domain.Tool{kept, text}))
	require.False(t, call(t, c, "kept", nil).IsError)
	require.Equal(t, "unwrapped", call(t, c, "text", nil).Content[0].(*mcp.TextContent).Text)
	listed, err := c.ListTools(t.Context(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	require.ElementsMatch(t, []string{"reserved", "kept", "text"}, names)
	for _, output := range []string{`null`, `{}`, `false`} {
		text.Handler = func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(output), nil }
		require.NoError(t, h.ReplaceSource("remote", []domain.Tool{text}))
		require.True(t, call(t, c, "text", nil).IsError)
	}
	require.NoError(t, h.ReplaceSource("remote", nil))
	listed, err = c.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, 1)
	require.Equal(t, "reserved", listed.Tools[0].Name)
}
