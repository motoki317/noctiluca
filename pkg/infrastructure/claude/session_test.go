// Claude CLI protocol reference: OpenClaw (MIT), https://github.com/openclaw/openclaw.
// Go reimplementation. Provenance and license notice: THIRD_PARTY_NOTICES.md.

package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/stretchr/testify/require"
)

const savedSessionID = "11111111-1111-4111-8111-111111111111"

type invocation struct {
	Args                               []string
	MCP                                json.RawMessage
	Prompt                             string
	FileMode, DirMode                  uint32
	HasAPIKey, HasAuthToken, HasSimple bool
	ChildPID                           int
}

func fakeCLI(t *testing.T, mode string) (string, string) {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("NOCTILUCA_TEST_BINARY", binary)
	t.Setenv("NOCTILUCA_TEST_MODE", mode)
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexec \"$NOCTILUCA_TEST_BINARY\" -test.run=TestCLIHelper -- \"$@\"\n"), 0o700))
	return path, dir
}

func TestCLIHelper(t *testing.T) {
	mode := os.Getenv("NOCTILUCA_TEST_MODE")
	if mode == "" {
		return
	}
	if mode == "child" {
		signal.Ignore(syscall.SIGTERM)
		// A descendant must not keep Close blocked when it ignores SIGTERM.
		for {
			_ = syscall.Pause()
		}
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) == 1 && args[0] == "--version" {
		if mode == "version-descendant" {
			child := exec.Command(os.Getenv("NOCTILUCA_TEST_BINARY"), "-test.run=TestCLIHelper")
			child.Env = append(os.Environ(), "NOCTILUCA_TEST_MODE=child")
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if child.Start() != nil {
				os.Exit(10)
			}
			_ = os.WriteFile(os.Getenv("NOCTILUCA_TEST_VERSION_CAPTURE"), []byte(strconv.Itoa(child.Process.Pid)), 0600)
			for {
				_ = syscall.Pause()
			}
		}
		version := MinimumVersion
		if override := os.Getenv("NOCTILUCA_TEST_VERSION"); override != "" {
			version = override
		}
		fmt.Println(version + " (Claude Code)")
		os.Exit(0)
	}
	get := func(flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	inv := invocation{Args: args, HasAPIKey: os.Getenv("ANTHROPIC_API_KEY") != "", HasAuthToken: os.Getenv("ANTHROPIC_AUTH_TOKEN") != "", HasSimple: os.Getenv("CLAUDE_CODE_SIMPLE") != ""}
	inv.MCP, _ = os.ReadFile(get("--mcp-config"))
	stat, _ := os.Stat(get("--mcp-config"))
	inv.FileMode = uint32(stat.Mode().Perm())
	stat, _ = os.Stat(filepath.Dir(get("--mcp-config")))
	inv.DirMode = uint32(stat.Mode().Perm())
	if p := get("--append-system-prompt-file"); p != "" {
		b, _ := os.ReadFile(p)
		inv.Prompt = string(b)
	}
	if mode == "descendant" || mode == "orphan" {
		if mode == "descendant" {
			signal.Ignore(syscall.SIGTERM)
		}
		child := exec.Command(os.Getenv("NOCTILUCA_TEST_BINARY"), "-test.run=TestCLIHelper")
		child.Env = append(os.Environ(), "NOCTILUCA_TEST_MODE=child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(10)
		}
		inv.ChildPID = child.Process.Pid
	}
	capture, _ := json.Marshal(inv)
	_ = os.WriteFile("invocation.json", capture, 0o600)
	records, err := os.OpenFile("invocations.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(12)
	}
	_, _ = records.Write(append(capture, '\n'))
	_ = records.Close()
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), maxFrameSize)
	emit := func(line string) { fmt.Println(line) }
	startupFailure := os.Getenv("NOCTILUCA_TEST_INIT_ERROR")
	if mode == "missing-resume" {
		if token := get("--resume"); token != "" {
			startupFailure = fmt.Sprintf(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":[%q]}`, "No conversation found with session ID: "+token)
		}
	}
	if mode == "init-blocked" {
		for {
			_ = syscall.Pause()
		}
	}
	if startupFailure != "" || mode == "init-exit" {
		if os.Getenv("NOCTILUCA_TEST_INIT_AFTER_READ") == "1" {
			scan.Scan()
		}
		fmt.Fprintln(os.Stderr, "private stderr: No conversation found with session ID: "+savedSessionID)
		emit(startupFailure)
		os.Exit(1)
	}
	count := 0
	var held, queued string
	for scan.Scan() {
		var f map[string]json.RawMessage
		if json.Unmarshal(scan.Bytes(), &f) != nil {
			os.Exit(11)
		}
		var typ string
		_ = json.Unmarshal(f["type"], &typ)
		switch typ {
		case "control_request":
			if mode == "init-error" {
				emit(`{"type":"control_response","response":{"request_id":"initialize","subtype":"error"}}`)
				continue
			}
			emit(`{"type":"control_response","response":{"request_id":"initialize","subtype":"success"}}`)
			emit(`{"type":"system","subtype":"init","session_id":"test-session"}`)
			if mode == "blocked" || mode == "descendant" {
				for {
					_ = syscall.Pause()
				}
			}
		case "user":
			count++
			if mode == "idle" {
				var id string
				_ = json.Unmarshal(f["uuid"], &id)
				lifecycle := func(id, state string) {
					emit(fmt.Sprintf(`{"type":"command_lifecycle","command_uuid":%q,"state":%q}`, id, state))
				}
				switch count {
				case 1:
					held = id
					lifecycle(id, "started")
				case 2:
					queued = id
					lifecycle(id, "queued")
					emit(`{"type":"result","subtype":"success"}`)
					lifecycle(held, "completed")
					emit(`{"type":"result","subtype":"success","origin":{"kind":"task-notification"}}`)
				case 3:
					lifecycle(queued, "started")
					lifecycle(queued, "cancelled")
					lifecycle(id, "refused")
				case 4:
					lifecycle(id, "started")
					emit(`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"task"}]}`)
					emit(`{"type":"system","subtype":"task_started","task_id":"task","is_backgrounded":true}`)
					emit(`{"type":"result","subtype":"success"}`)
					lifecycle(id, "completed")
				case 5:
					emit(`{"type":"system","subtype":"task_notification","task_id":"task","status":"completed"}`)
					lifecycle(id, "completed")
				}
				emit(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"checkpoint"}}}`)
				continue
			}
			_ = os.WriteFile("input-"+strconv.Itoa(count)+".json", scan.Bytes(), 0o600)
			switch mode {
			case "malformed":
				emit("not-json")
				continue
			case "oversized":
				emit(strings.Repeat("x", maxFrameSize))
				continue
			case "exit", "orphan":
				os.Exit(3)
			case "flood":
				for i := 0; i < 1000; i++ {
					emit(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}}`)
				}
			case "permission":
				emit(`{"type":"control_request","request_id":"permission","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"anything"}}}`)
				continue
			}
			if mode == "steer" && count == 2 {
				emit(`{"type":"result","subtype":"success"}`)
			}
			emit(`{"type":"command_lifecycle","state":"started"}`)
			emit(`{"type":"user","isReplay":true,"message":{"role":"user","content":"replayed input"}}`)
			// Exercise a UTF-8 frame split across writes.
			fmt.Print(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"`)
			emit(`日本語"}}}`)
			if mode == "steer" && count == 1 {
				continue
			}
			emit(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-1","name":"reply","input":{"text":"ok"}}]}}`)
			emit(`{"type":"result","subtype":"success","session_id":"test-session"}`)
			if mode == "steer" {
				emit(`{"type":"system","subtype":"task_notification","task_id":"task-1","status":"completed","summary":"done"}`)
			}
		case "control_response":
			_ = os.WriteFile("permission.json", scan.Bytes(), 0o600)
			emit(`{"type":"result","subtype":"success"}`)
		}
	}
	os.Exit(0)
}

func TestSessionIdle(t *testing.T) {
	path, dir := fakeCLI(t, "idle")
	s, err := New(path, "haiku").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	for _, idle := range []bool{false, false, true, false, true} {
		require.NoError(t, s.Send(t.Context(), domain.UserInput{Text: "next"}))
		for nextEvent(t, s).Text != "checkpoint" {
		}
		require.Equal(t, idle, s.Idle())
	}
}

func TestVersionCompatibility(t *testing.T) {
	for _, tc := range []struct {
		version     string
		wantError   bool
		wantWarning bool
	}{
		{version: "1.999.999", wantError: true},
		{version: "2.0.999", wantError: true},
		{version: "2.1.315", wantError: true},
		{version: "2.1.316"},
		{version: "2.1.317"},
		{version: "2.1.1000"},
		{version: "2.2.0", wantWarning: true},
		{version: "2.10.0", wantWarning: true},
		{version: "3.0.0", wantWarning: true},
		{version: "invalid", wantError: true},
		{version: "2.1", wantError: true},
		{version: "2.1.-1", wantError: true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			path, dir := fakeCLI(t, "echo")
			t.Setenv("NOCTILUCA_TEST_VERSION", tc.version)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			s, err := New(path, "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir})
			if tc.wantError {
				require.Error(t, err)
				require.Nil(t, s)
			} else {
				require.NoError(t, err)
				require.NoError(t, s.Close())
			}
			if tc.wantWarning {
				var record struct {
					Level, Version  string
					MeasuredVersion string `json:"measured_version"`
				}
				require.NoError(t, json.Unmarshal(logs.Bytes(), &record))
				require.Equal(t, "WARN", record.Level)
				require.Equal(t, tc.version, record.Version)
				require.Equal(t, MinimumVersion, record.MeasuredVersion)
			} else {
				require.Empty(t, logs.String())
			}
		})
	}
}

func readInvocation(t *testing.T, dir string) invocation {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "invocation.json"))
	require.NoError(t, err)
	var inv invocation
	require.NoError(t, json.Unmarshal(data, &inv))
	return inv
}

func nextEvent(t *testing.T, s domain.ModelSession) domain.ModelEvent {
	t.Helper()
	select {
	case event, ok := <-s.Events():
		require.True(t, ok, "event stream closed")
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("event timeout")
		return domain.ModelEvent{}
	}
}

func TestSessionSteerResumeAndConfig(t *testing.T) {
	path, dir := fakeCLI(t, "steer")
	t.Setenv("ANTHROPIC_API_KEY", "fake-secret")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "fake-token")
	t.Setenv("CLAUDE_CODE_SIMPLE", "1")
	s, err := New(path, "haiku").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir, ResumeToken: savedSessionID, SystemPrompt: "Owner instructions", ToolHub: domain.MCPEndpoint{URL: "http://127.0.0.1:1234/mcp", BearerToken: "test-bearer"}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	inv := readInvocation(t, dir)
	require.False(t, inv.HasAPIKey)
	require.False(t, inv.HasAuthToken)
	require.False(t, inv.HasSimple)
	require.EqualValues(t, 0o600, inv.FileMode)
	require.EqualValues(t, 0o700, inv.DirMode)
	require.Equal(t, "Owner instructions", inv.Prompt)
	require.JSONEq(t, `{"mcpServers":{"noctiluca":{"type":"http","url":"http://127.0.0.1:1234/mcp","headers":{"Authorization":"Bearer test-bearer"}}}}`, string(inv.MCP))
	joined := strings.Join(inv.Args, " ")
	require.NotContains(t, joined, "--bare")
	require.NotContains(t, joined, "test-bearer")
	require.True(t, s.Idle())
	for _, flag := range []string{"--resume " + savedSessionID, "--model haiku", "--setting-sources user,project,local", "--permission-mode default", "--strict-mcp-config", "--allowedTools mcp__noctiluca__*"} {
		require.Contains(t, joined, flag)
	}
	require.NoError(t, s.Send(t.Context(), domain.UserInput{Text: "first"}))
	require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
	require.Equal(t, "日本語", nextEvent(t, s).Text)
	require.Equal(t, "test-session", s.ResumeToken())
	require.NoError(t, s.Send(t.Context(), domain.UserInput{Text: "second", Attachments: []domain.Attachment{{Ref: "file-ref", Name: "photo"}}}))
	for _, kind := range []domain.ModelEventKind{domain.ModelEventTurnFinished, domain.ModelEventTurnStarted, domain.ModelEventText, domain.ModelEventToolCall, domain.ModelEventTurnFinished, domain.ModelEventBackgroundDone} {
		require.Equal(t, kind, nextEvent(t, s).Kind)
	}
	data, err := os.ReadFile(filepath.Join(dir, "input-2.json"))
	require.NoError(t, err)
	var input struct {
		Type, UUID string
		SessionID  string `json:"session_id"`
		Parent     any    `json:"parent_tool_use_id"`
		Message    struct{ Role, Content string }
	}
	require.NoError(t, json.Unmarshal(data, &input))
	require.Equal(t, "user", input.Type)
	require.Equal(t, "user", input.Message.Role)
	require.Nil(t, input.Parent)
	require.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, input.UUID)
	require.Equal(t, "test-session", input.SessionID)
	require.Contains(t, input.Message.Content, "file-ref")
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	require.Error(t, s.Send(t.Context(), domain.UserInput{Text: "closed"}))
	for i, a := range inv.Args {
		if a == "--mcp-config" {
			_, err := os.Stat(filepath.Dir(inv.Args[i+1]))
			require.True(t, os.IsNotExist(err))
		}
	}
}

func readInvocations(t *testing.T, dir string) []invocation {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "invocations.jsonl"))
	require.NoError(t, err)
	var invocations []invocation
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var inv invocation
		require.NoError(t, json.Unmarshal(line, &inv))
		invocations = append(invocations, inv)
	}
	return invocations
}

func TestCodexResumeStartsFresh(t *testing.T) {
	path, dir := fakeCLI(t, "echo")
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	token := savedSessionID + ":10086c95f9520188"
	s, err := New(path, "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir, ResumeToken: token})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	invocations := readInvocations(t, dir)
	require.Len(t, invocations, 1)
	require.NotContains(t, invocations[0].Args, "--resume")
	require.NotEqual(t, token, s.ResumeToken())
	require.NoError(t, s.Send(t.Context(), domain.UserInput{Text: "fresh input"}))
	for nextEvent(t, s).Kind != domain.ModelEventTurnFinished {
	}
	require.Equal(t, "test-session", s.ResumeToken())
	var record struct{ Level, Msg string }
	require.NoError(t, json.Unmarshal(logs.Bytes(), &record))
	require.Equal(t, "WARN", record.Level)
	require.Contains(t, record.Msg, "fresh session")
	require.NotContains(t, logs.String(), token)
}

func TestMissingResumeStartsFresh(t *testing.T) {
	for _, afterRead := range []string{"0", "1"} {
		t.Run("after-read-"+afterRead, func(t *testing.T) {
			path, dir := fakeCLI(t, "missing-resume")
			t.Setenv("NOCTILUCA_TEST_INIT_AFTER_READ", afterRead)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			config := domain.ModelSessionConfig{Workdir: dir, ResumeToken: savedSessionID, SystemPrompt: "Owner instructions", ToolHub: domain.MCPEndpoint{URL: "http://127.0.0.1:1234/mcp", BearerToken: "test-bearer"}}
			s, err := New(path, "haiku").Open(t.Context(), config)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			require.NotEqual(t, config.ResumeToken, s.ResumeToken())
			require.NoError(t, s.Send(t.Context(), domain.UserInput{Text: "fresh input"}))
			for nextEvent(t, s).Kind != domain.ModelEventTurnFinished {
			}
			require.Equal(t, "test-session", s.ResumeToken())
			invocations := readInvocations(t, dir)
			require.Len(t, invocations, 2)
			require.Contains(t, strings.Join(invocations[0].Args, " "), "--resume "+savedSessionID)
			require.NotContains(t, invocations[1].Args, "--resume")
			for _, inv := range invocations {
				require.Contains(t, strings.Join(inv.Args, " "), "--model haiku")
				require.Equal(t, config.SystemPrompt, inv.Prompt)
				require.JSONEq(t, `{"mcpServers":{"noctiluca":{"type":"http","url":"http://127.0.0.1:1234/mcp","headers":{"Authorization":"Bearer test-bearer"}}}}`, string(inv.MCP))
			}
			require.NoError(t, s.Close())
			for _, inv := range invocations {
				for i, arg := range inv.Args {
					if arg == "--mcp-config" {
						require.NoDirExists(t, filepath.Dir(inv.Args[i+1]))
					}
				}
			}
			var record struct{ Level, Msg string }
			require.NoError(t, json.Unmarshal(logs.Bytes(), &record))
			require.Equal(t, "WARN", record.Level)
			require.Contains(t, record.Msg, "fresh session")
			require.NotContains(t, logs.String(), "private stderr")
			require.NotContains(t, logs.String(), "test-bearer")
		})
	}
}

func TestInitializationExitKeepsResume(t *testing.T) {
	for _, tc := range []struct{ name, frame, detail string }{
		{"auth", `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["Authentication required","Run login"]}`, "Authentication required; Run login"},
		{"crash", "", authRemedy},
		{"other-session", `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["No conversation found with session ID: other-session"]}`, "other-session"},
		{"result-body", `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"No conversation found with session ID: ` + savedSessionID + `"}`, authRemedy},
		{"other-frame", `{"type":"system","subtype":"notice","errors":["No conversation found with session ID: ` + savedSessionID + `"]}`, authRemedy},
		{"successful-result", `{"type":"result","subtype":"success","errors":["No conversation found with session ID: ` + savedSessionID + `"]}`, authRemedy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, afterRead := range []string{"0", "1"} {
				t.Run("after-read-"+afterRead, func(t *testing.T) {
					path, dir := fakeCLI(t, "init-exit")
					t.Setenv("NOCTILUCA_TEST_INIT_ERROR", tc.frame)
					t.Setenv("NOCTILUCA_TEST_INIT_AFTER_READ", afterRead)
					var logs bytes.Buffer
					previous := slog.Default()
					slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
					t.Cleanup(func() { slog.SetDefault(previous) })
					config := domain.ModelSessionConfig{Workdir: dir, ResumeToken: savedSessionID}
					s, err := New(path, "").Open(t.Context(), config)
					require.ErrorContains(t, err, "claude exited before initialization")
					require.ErrorContains(t, err, tc.detail)
					require.NotContains(t, err.Error(), "context canceled")
					require.NotContains(t, err.Error(), "private stderr")
					require.Nil(t, s)
					invocations := readInvocations(t, dir)
					require.Len(t, invocations, 1)
					require.Contains(t, strings.Join(invocations[0].Args, " "), "--resume "+savedSessionID)
					require.Equal(t, savedSessionID, config.ResumeToken)
					require.Empty(t, logs.String())
				})
			}
		})
	}
}

func TestMissingResumeFallbackFailure(t *testing.T) {
	for _, detail := range []string{"Authentication required", "No conversation found with session ID: " + savedSessionID} {
		t.Run(detail, func(t *testing.T) {
			path, dir := fakeCLI(t, "missing-resume")
			t.Setenv("NOCTILUCA_TEST_INIT_ERROR", fmt.Sprintf(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":[%q]}`, detail))
			s, err := New(path, "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir, ResumeToken: savedSessionID})
			require.Nil(t, s)
			require.ErrorContains(t, err, "claude exited before initialization: "+detail)
			invocations := readInvocations(t, dir)
			require.Len(t, invocations, 2)
			require.Contains(t, strings.Join(invocations[0].Args, " "), "--resume "+savedSessionID)
			require.NotContains(t, invocations[1].Args, "--resume")
		})
	}
}

func TestInitializationCancellationKeepsCause(t *testing.T) {
	path, dir := fakeCLI(t, "init-blocked")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := New(path, "").Open(ctx, domain.ModelSessionConfig{Workdir: dir, ResumeToken: savedSessionID})
		done <- err
	}()
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "invocations.jsonl"))
		return err == nil && json.Valid(bytes.TrimSpace(data))
	}, 5*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Len(t, readInvocations(t, dir), 1)
}

func TestWriteFailureKeepsCause(t *testing.T) {
	input, output, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, input.Close())
	t.Cleanup(func() { require.NoError(t, output.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &session{ctx: ctx, cancel: cancel, stdin: output, writeLock: make(chan struct{}, 1)}
	err = s.write(ctx, map[string]string{"type": "control_request"})
	require.EqualError(t, err, "write claude input failed")
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

func TestSessionFailures(t *testing.T) {
	for _, mode := range []string{"malformed", "oversized", "exit", "orphan", "permission"} {
		t.Run(mode, func(t *testing.T) {
			path, dir := fakeCLI(t, mode)
			s, err := New(path, "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			require.NoError(t, s.Send(t.Context(), domain.UserInput{Text: "test"}))
			event := nextEvent(t, s)
			if mode == "permission" {
				require.Equal(t, domain.ModelEventTurnFinished, event.Kind)
				data, err := os.ReadFile(filepath.Join(dir, "permission.json"))
				require.NoError(t, err)
				require.JSONEq(t, `{"type":"control_response","response":{"subtype":"success","request_id":"permission","response":{"behavior":"allow","updatedInput":{"command":"anything"}}}}`, string(data))
			} else {
				require.Equal(t, domain.ModelEventError, event.Kind)
				require.Error(t, event.Err)
				if mode == "exit" || mode == "orphan" {
					require.Contains(t, event.Err.Error(), authRemedy)
				}
			}
		})
	}
	t.Run("init-error", func(t *testing.T) {
		path, dir := fakeCLI(t, "init-error")
		_, err := New(path, "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir})
		require.Error(t, err)
	})
}

func TestSessionCancellation(t *testing.T) {
	for _, mode := range []string{"blocked", "flood", "descendant"} {
		t.Run(mode, func(t *testing.T) {
			path, dir := fakeCLI(t, mode)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s, err := New(path, "").Open(ctx, domain.ModelSessionConfig{Workdir: dir})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			if mode == "blocked" {
				sendCtx, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer stop()
				require.ErrorIs(t, s.Send(sendCtx, domain.UserInput{Text: strings.Repeat("x", 1<<20)}), context.DeadlineExceeded)
			}
			if mode == "flood" {
				require.NoError(t, s.Send(t.Context(), domain.UserInput{Text: "flood"}))
				require.Equal(t, domain.ModelEventText, nextEvent(t, s).Kind)
			}
			cancel()
			done := make(chan error, 1)
			go func() { done <- s.Close() }()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(4 * time.Second):
				t.Fatal("Close blocked")
			}
			if mode == "descendant" {
				pid := readInvocation(t, dir).ChildPID
				// A killed process closes its stdout before it becomes a zombie, so Close can
				// return while the orphan is still exiting. The host's reaper may also leave it a zombie.
				require.Eventually(t, func() bool {
					data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
					if err != nil {
						return os.IsNotExist(err)
					}
					return strings.Contains(string(data), ") Z ")
				}, 2*time.Second, 10*time.Millisecond)
			}
		})
	}
}

func TestConcurrentSend(t *testing.T) {
	path, dir := fakeCLI(t, "echo")
	s, err := New(path, "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir})
	require.NoError(t, err)
	defer s.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Go(func() { errs <- s.Send(t.Context(), domain.UserInput{Text: "concurrent"}) })
	}
	for i := 0; i < 20; i++ {
		for _, kind := range []domain.ModelEventKind{domain.ModelEventTurnStarted, domain.ModelEventText, domain.ModelEventToolCall, domain.ModelEventTurnFinished} {
			require.Equal(t, kind, nextEvent(t, s).Kind)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func TestVersionProbeCancellationKillsDescendants(t *testing.T) {
	path, dir := fakeCLI(t, "version-descendant")
	capture := filepath.Join(dir, "child.pid")
	t.Setenv("NOCTILUCA_TEST_VERSION_CAPTURE", capture)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := New(path, "").Open(ctx, domain.ModelSessionConfig{}); done <- err }()
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(capture)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(string(data))
		return err == nil
	}, time.Second, time.Millisecond)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("version probe did not stop")
	}
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		return os.IsNotExist(err) || (err == nil && strings.Contains(string(data), ") Z "))
	}, time.Second, time.Millisecond)
}
