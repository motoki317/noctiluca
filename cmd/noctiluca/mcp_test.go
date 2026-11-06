package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPConfigAndLoginSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("mcp_servers:\n  - name: tracker\n    url: https://tracker.example/mcp\n    scopes: [read, offline_access]\n    tools: [search]\n  - name: wiki\n    url: https://wiki.example/mcp\n"), 0o600))
	config, err := loadConfig(path)
	require.NoError(t, err)
	require.Len(t, config.MCPServers, 2)

	assert.Equal(t, []string{"read", "offline_access"}, config.MCPServers[0].Scopes)
	assert.Equal(t, []string{"search"}, config.MCPServers[0].Tools)

	assert.Nil(t, config.MCPServers[1].Tools)
	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--config", path, "mcp-login", "missing"})
	require.ErrorContains(t, cmd.Execute(), "not configured")
	assert.Empty(t, out.String())
	cmd = newRootCommand()
	cmd.SetArgs([]string{"mcp-login"})
	require.Error(t, cmd.Execute())
}

func TestMCPConfigRejectsRemovedMembersSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("mcp_servers:\n  - name: tracker\n    url: https://tracker.example/mcp\n    members: true\n"), 0600))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "invalid config")
}
