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
		args     []string
		env      *string
		wantLog  bool
		wantErr  string
	}{
		{name: "defaults", wantLog: true},
		{name: "empty file", file: "config.yaml", wantLog: true},
		{name: "default file", file: "config.yaml", contents: "log:\n  level: WARN\n"},
		{name: "explicit file", file: "custom.yaml", contents: "log:\n  level: ERROR\n", args: []string{"--config", "custom.yaml"}},
		{name: "environment only", env: new("ERROR")},
		{name: "environment overrides file", file: "config.yaml", contents: "log:\n  level: ERROR\n", env: new("INFO"), wantLog: true},
		{name: "empty environment", env: new(""), wantErr: "invalid log.level"},
		{name: "missing explicit file", args: []string{"--config", "missing.yaml"}, wantErr: "read config"},
		{name: "invalid YAML", file: "config.yaml", contents: "log: [secret-value", wantErr: "invalid config"},
		{name: "scalar YAML", file: "config.yaml", contents: "secret-value", wantErr: "invalid config"},
		{name: "unknown field", file: "config.yaml", contents: "token: secret-value\n", wantErr: "invalid config"},
		{name: "invalid field type", file: "config.yaml", contents: "log:\n  level: [secret-value]\n", wantErr: "invalid config"},
		{name: "invalid level", file: "config.yaml", contents: "log:\n  level: secret-value\n", wantErr: "invalid log.level"},
		{name: "invalid environment level", env: new("secret-value"), wantErr: "invalid log.level"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if tt.file != "" {
				require.NoError(t, os.WriteFile(tt.file, []byte(tt.contents), 0o600))
			}
			if tt.env != nil {
				t.Setenv("NOCTILUCA_LOG_LEVEL", *tt.env)
			}
			path := ""
			if len(tt.args) == 2 {
				path = tt.args[1]
			}
			config, err := loadConfig(path)
			if tt.wantErr == "" {
				require.NoError(t, err)
				if tt.wantLog {
					assert.Equal(t, "INFO", config.Log.Level)
				}
			} else {
				require.ErrorContains(t, err, tt.wantErr)
				assert.NotContains(t, err.Error(), "secret-value")
			}
		})
	}
}

func TestVersionAndHelpSkipConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.yaml")
	require.NoError(t, os.WriteFile(path, []byte("log: ["), 0o600))
	t.Setenv("NOCTILUCA_LOG_LEVEL", "invalid")
	for _, tt := range []struct {
		args []string
		want string
	}{
		{args: []string{"version"}, want: "dev (unknown)\n"},
		{args: []string{"--version"}, want: "noctiluca version dev (unknown)\n"},
		{args: []string{"--help"}, want: "Usage:"},
	} {
		t.Run(tt.args[0], func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := newRootCommand()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(append([]string{"--config", path}, tt.args...))
			require.NoError(t, cmd.Execute())
			assert.Contains(t, stdout.String(), tt.want)
			assert.Empty(t, stderr.String())
		})
	}
}

func TestRejectUnexpectedArguments(t *testing.T) {
	for _, args := range [][]string{{"run", "extra"}, {"version", "extra"}, {"unknown"}, {"run", "--unknown"}} {
		cmd := newRootCommand()
		cmd.SetArgs(args)
		require.Error(t, cmd.Execute())
	}
}
