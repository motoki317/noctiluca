package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunConfig(t *testing.T) {
	t.Setenv("NOCTILUCA_DATABASE_PATH", "")
	require.NoError(t, os.Unsetenv("NOCTILUCA_DATABASE_PATH"))
	t.Setenv("NOCTILUCA_LOG_LEVEL", "")
	require.NoError(t, os.Unsetenv("NOCTILUCA_LOG_LEVEL"))

	for _, tt := range []struct {
		name     string
		file     string
		contents string
		path     string
		env      *string
		wantLog  string
		wantErr  string
	}{
		{name: "defaults", wantLog: "INFO"},
		{name: "default file", file: "config.yaml", contents: "log:\n  level: WARN\n", wantLog: "WARN"},
		{name: "explicit file", file: "custom.yaml", contents: "log:\n  level: ERROR\n", path: "custom.yaml", wantLog: "ERROR"},
		{name: "environment overrides file", file: "config.yaml", contents: "log:\n  level: ERROR\n", env: new("INFO"), wantLog: "INFO"},
		{name: "empty environment", env: new(""), wantErr: "invalid log.level"},
		{name: "missing explicit file", path: "missing.yaml", wantErr: "read config"},
		{name: "invalid YAML", file: "config.yaml", contents: "log: [secret-value", wantErr: "invalid config"},
		{name: "scalar YAML", file: "config.yaml", contents: "secret-value", wantErr: "invalid config"},
		{name: "unknown field", file: "config.yaml", contents: "token: secret-value\n", wantErr: "invalid config"},
		{name: "invalid field type", file: "config.yaml", contents: "log:\n  level: [secret-value]\n", wantErr: "invalid config"},
		{name: "invalid level", file: "config.yaml", contents: "log:\n  level: secret-value\n", wantErr: "invalid log.level"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if tt.file != "" {
				require.NoError(t, os.WriteFile(tt.file, []byte(tt.contents), 0o600))
			}
			if tt.env != nil {
				t.Setenv("NOCTILUCA_LOG_LEVEL", *tt.env)
			}
			config, err := loadConfig(tt.path)
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, tt.wantLog, config.Log.Level)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
				assert.NotContains(t, err.Error(), "secret-value")
			}
		})
	}
}

func TestVersionSkipsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.yaml")
	require.NoError(t, os.WriteFile(path, []byte("log: ["), 0o600))
	t.Setenv("NOCTILUCA_LOG_LEVEL", "invalid")
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--config", path, "version"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "dev (unknown)\n", stdout.String())
	assert.Empty(t, stderr.String())
}
