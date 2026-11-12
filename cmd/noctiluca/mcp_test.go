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
