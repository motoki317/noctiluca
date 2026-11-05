// Claude CLI protocol reference: OpenClaw (MIT), https://github.com/openclaw/openclaw.
// Go reimplementation. Provenance and license notice: THIRD_PARTY_NOTICES.md.

package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

// MinimumVersion is the oldest CLI whose stream protocol was measured.
const MinimumVersion = "2.1.316"

type adapter struct{ executable, model string }

// New uses the CLI's configured model when model is empty.
func New(executable, model string) domain.ModelAdapter {
	if executable == "" {
		executable = "claude"
	}
	return &adapter{executable: executable, model: model}
}

func childEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDECODE", "CLAUDE_CODE_SIMPLE":
			continue
		}
		env = append(env, entry)
	}
	return env
}

func (a *adapter) Open(ctx context.Context, config domain.ModelSessionConfig) (domain.ModelSession, error) {
	versionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	version := exec.CommandContext(versionCtx, a.executable, "--version")
	version.Env = childEnv()
	version.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	version.WaitDelay = 100 * time.Millisecond
	version.Cancel = func() error {
		err := syscall.Kill(-version.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	output, err := version.Output()
	if version.Process != nil {
		_ = syscall.Kill(-version.Process.Pid, syscall.SIGKILL)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("check claude version failed")
	}
	if err := checkVersion(ctx, string(output)); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "noctiluca-claude-")
	if err != nil {
		return nil, fmt.Errorf("create claude config directory: %w", err)
	}
	opened := false
	defer func() {
		if !opened {
			_ = os.RemoveAll(dir)
		}
	}()
	args := []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--replay-user-messages", "--permission-prompt-tool", "stdio",
		"--permission-mode", "default", "--setting-sources", "user,project,local", "--strict-mcp-config"}
	if a.model != "" {
		args = append(args, "--model", a.model)
	}
	if config.ResumeToken != "" {
		args = append(args, "--resume", config.ResumeToken)
	}
	if config.SystemPrompt != "" {
		path := filepath.Join(dir, "prompt.txt")
		if err := os.WriteFile(path, []byte(config.SystemPrompt), 0o600); err != nil {
			return nil, fmt.Errorf("write claude prompt: %w", err)
		}
		args = append(args, "--append-system-prompt-file", path)
	}
	servers := map[string]any{}
	if config.ToolHub.URL != "" {
		servers["noctiluca"] = map[string]any{"type": "http", "url": config.ToolHub.URL, "headers": map[string]string{"Authorization": "Bearer " + config.ToolHub.BearerToken}}
		// The hub owns authorization for these tools, including calls from subagents.
		args = append(args, "--allowedTools", "mcp__noctiluca__*")
	}
	data, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return nil, fmt.Errorf("encode claude MCP config: %w", err)
	}
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, fmt.Errorf("write claude MCP config: %w", err)
	}
	args = append(args, "--mcp-config", path)
	s, err := startSession(ctx, a.executable, args, config, dir)
	if err != nil {
		return nil, err
	}
	opened = true
	return s, nil
}

func checkVersion(ctx context.Context, output string) error {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return errors.New("claude returned no version")
	}
	version, err := parseVersion(fields[0])
	if err != nil {
		return err
	}
	minimum, _ := parseVersion(MinimumVersion)
	if slices.Compare(version[:], minimum[:]) < 0 {
		return fmt.Errorf("claude version must be at least %s", MinimumVersion)
	}
	if version[0] > minimum[0] || version[1] > minimum[1] {
		slog.WarnContext(ctx, "Claude major or minor version exceeds the measured version; verify stream protocol compatibility",
			"version", fields[0], "measured_version", MinimumVersion)
	}
	return nil
}

func parseVersion(value string) ([3]uint64, error) {
	var version [3]uint64
	parts := strings.Split(value, ".")
	if len(parts) != len(version) {
		return version, errors.New("cannot parse claude version: expected major.minor.patch")
	}
	for i, part := range parts {
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return version, errors.New("cannot parse claude version: expected numeric components")
		}
		version[i] = n
	}
	return version, nil
}
