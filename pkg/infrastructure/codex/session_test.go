package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func fakeServer(t *testing.T, mode string) (string, string) {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("NOCTILUCA_CODEX_TEST_BINARY", binary)
	t.Setenv("NOCTILUCA_CODEX_TEST_MODE", mode)
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexec \"$NOCTILUCA_CODEX_TEST_BINARY\" -test.run=TestAppServerHelper -- \"$@\"\n"), 0700))
	return path, dir
}

func TestAppServerHelper(t *testing.T) {
	mode := os.Getenv("NOCTILUCA_CODEX_TEST_MODE")
	if mode == "" {
		return
	}
	if mode == "child" {
		signal.Ignore(syscall.SIGTERM)
		for {
			_ = syscall.Pause()
		}
	}
	if mode == "descendant" {
		signal.Ignore(syscall.SIGTERM)
		child := exec.Command(os.Getenv("NOCTILUCA_CODEX_TEST_BINARY"), "-test.run=TestAppServerHelper")
		child.Env = append(os.Environ(), "NOCTILUCA_CODEX_TEST_MODE=child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if child.Start() != nil {
			os.Exit(10)
		}
		_ = os.WriteFile("child.pid", []byte(strconv.Itoa(child.Process.Pid)), 0600)
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	capture, _ := json.Marshal(map[string]any{"args": args, "codexHome": os.Getenv("CODEX_HOME")})
	_ = os.WriteFile("invocation.json", capture, 0600)
	log, err := os.OpenFile("requests.jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(11)
	}
	defer log.Close()
	emit := func(value any) { data, _ := json.Marshal(value); fmt.Println(string(data)) }
	notify := func(method string, params any) { emit(map[string]any{"method": method, "params": params}) }
	turnNote := func(method, status string) {
		notify(method, map[string]any{"threadId": "thread-1", "turn": map[string]string{"id": "turn-1", "status": status}})
	}
	initialized := false
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 64<<10), maxFrameSize)
	for scan.Scan() {
		_, _ = log.Write(append(append([]byte(nil), scan.Bytes()...), '\n'))
		var f rpcFrame
		if json.Unmarshal(scan.Bytes(), &f) != nil {
			os.Exit(12)
		}
		respond := func(value any) { emit(map[string]any{"id": f.ID, "result": value}) }
		reject := func(message string) { emit(rpcFrame{ID: f.ID, Error: &rpcError{Code: -32600, Message: message}}) }
		switch f.Method {
		case "initialize":
			if mode == "init-blocked" {
				continue
			}
			if mode == "init-exit" {
				os.Exit(0)
			}
			if mode == "malformed" {
				fmt.Println("invalid frame secret-bearer")
				continue
			}
			respond(map[string]string{"userAgent": "codex/0.163.0"})
		case "initialized":
			initialized = true
		case "thread/start", "thread/resume":
			if !initialized {
				os.Exit(13)
			}
			if mode == "missing" && f.Method == "thread/resume" {
				reject("no rollout found for thread ID missing")
				continue
			}
			if mode == "open-error" {
				reject("permission denied secret-bearer")
				continue
			}
			if mode == "injection-fresh-missing-id" && f.Method == "thread/start" {
				respond(map[string]any{})
				continue
			}
			respond(map[string]any{"thread": map[string]string{"id": "thread-1"}})
		case "thread/inject_items":
			switch mode {
			case "no-injection", "injection-fresh-missing-id":
				emit(rpcFrame{ID: f.ID, Error: &rpcError{Code: -32601, Message: "Method not found"}})
			case "unknown-injection":
				reject("Invalid request: unknown variant `thread/inject_items`, expected one of `initialize`, `thread/start`")
			case "injection-error":
				reject("invalid item")
			default:
				respond(map[string]any{})
			}
		case "turn/start":
			if mode == "reject" {
				reject("turn rejected")
				continue
			}
			if mode != "early-events" {
				respond(map[string]any{"turn": map[string]string{"id": "turn-1"}})
			}
			if mode == "response-only" {
				continue
			}
			turnNote("turn/started", "inProgress")
			if mode == "stay" || mode == "interrupt-blocked" {
				continue
			}
			if mode == "crash" {
				os.Exit(1)
			}
			if mode == "malformed-turn" {
				fmt.Println("not-json secret-bearer")
				continue
			}
			if mode == "rate-limit" {
				notify("error", map[string]any{"threadId": "thread-1", "turnId": "turn-1", "willRetry": false,
					"error": map[string]string{"message": "quota secret-bearer exhausted", "codexErrorInfo": "usageLimitExceeded"}})
				turnNote("turn/completed", "failed")
				continue
			}
			emit(map[string]string{"method": "future/notification"})
			notify("item/agentMessage/delta", map[string]string{"threadId": "other-thread", "delta": "wrong"})
			count := 1
			if mode == "early-events" {
				count = 100
			}
			for range count {
				notify("item/agentMessage/delta", map[string]string{"threadId": "thread-1", "delta": "hello"})
			}
			for _, method := range []string{"item/started", "item/completed"} {
				notify(method, map[string]any{"threadId": "thread-1", "turnId": "turn-1",
					"item": map[string]any{"type": "mcpToolCall", "id": "tool-1", "server": "noctiluca", "tool": "reply", "arguments": map[string]string{"text": "hello"}, "status": "completed"}})
			}
			turnNote("turn/completed", "completed")
			if mode == "early-events" {
				respond(map[string]any{"turn": map[string]string{"id": "turn-1"}})
			}
		case "turn/interrupt":
			if mode == "interrupt-blocked" {
				continue
			}
			respond(map[string]any{})
			turnNote("turn/completed", "interrupted")
		}
	}
	if mode == "descendant" {
		for {
			_ = syscall.Pause()
		}
	}
	os.Exit(0)
}

func requests(t *testing.T, dir string) []rpcFrame {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	require.NoError(t, err)
	var frames []rpcFrame
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var f rpcFrame
		require.NoError(t, json.Unmarshal(line, &f))
		frames = append(frames, f)
	}
	return frames
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

func TestOpenResumeAndSettings(t *testing.T) {
	token := "thread-1:" + promptHash(developerInstructions("System instructions"))
	for _, resume := range []string{"", token} {
		t.Run("resume="+resume, func(t *testing.T) {
			path, dir := fakeServer(t, "normal")
			t.Setenv("CODEX_HOME", filepath.Join(dir, "shared-home"))
			s, err := New(path, "test-model", "low").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir,
				ResumeToken: resume, SystemPrompt: "System instructions", ToolHub: domain.MCPEndpoint{URL: "http://127.0.0.1:1234/mcp", BearerToken: "secret-bearer"}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			require.True(t, s.Idle())
			require.Equal(t, token, s.ResumeToken())
			capture, err := os.ReadFile(filepath.Join(dir, "invocation.json"))
			require.NoError(t, err)
			var inv struct {
				Args      []string `json:"args"`
				CodexHome string   `json:"codexHome"`
			}
			require.NoError(t, json.Unmarshal(capture, &inv))
			require.Equal(t, []string{"app-server", "-c", "mcp_servers={}", "-c", "plugins={}"}, inv.Args)
			require.NotContains(t, string(capture), "secret-bearer")
			require.Equal(t, os.Getenv("CODEX_HOME"), inv.CodexHome)
			frames := requests(t, dir)
			require.Len(t, frames, 3)
			require.Equal(t, "initialize", frames[0].Method)
			require.Equal(t, "initialized", frames[1].Method)
			method := "thread/start"
			if resume != "" {
				method = "thread/resume"
			}
			require.Equal(t, method, frames[2].Method)
			var params threadParams
			require.NoError(t, json.Unmarshal(frames[2].Params, &params))
			threadID, _, _ := strings.Cut(resume, ":")
			require.Equal(t, threadID, params.ThreadID)
			require.Equal(t, resume != "", params.ExcludeTurns)
			require.Equal(t, dir, params.Cwd)
			require.Equal(t, "System instructions\n\n"+backgroundInstruction, params.DeveloperInstructions)
			require.Equal(t, "never", params.ApprovalPolicy)
			require.Equal(t, "danger-full-access", params.Sandbox)
			require.Equal(t, "test-model", params.Model)
			require.Equal(t, map[string]any{"features.apps": false, "model_reasoning_effort": "low", "mcp_servers.noctiluca": map[string]any{
				"url": "http://127.0.0.1:1234/mcp", "http_headers": map[string]any{"Authorization": "Bearer secret-bearer"}}}, params.Config)
		})
	}
}

func TestMissingThreadFallback(t *testing.T) {
	path, dir := fakeServer(t, "missing")
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	s, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir, ResumeToken: "missing"})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.Contains(t, logs.String(), "WARN")
	require.Contains(t, logs.String(), "starting a fresh session")
	require.Equal(t, "thread-1:"+promptHash(developerInstructions("")), s.ResumeToken())
	frames := requests(t, dir)
	require.Len(t, frames, 4)
	require.Equal(t, "thread/resume", frames[2].Method)
	require.Equal(t, "thread/start", frames[3].Method)
	var params map[string]any
	require.NoError(t, json.Unmarshal(frames[3].Params, &params))
	require.NotContains(t, params, "model")
	require.NotContains(t, params, "threadId")
	require.NotContains(t, params, "excludeTurns")
	require.Equal(t, map[string]any{"features.apps": false}, params["config"])
}

func TestTurnStreamAndRepeatedInput(t *testing.T) {
	for _, mode := range []string{"normal", "early-events"} {
		t.Run(mode, func(t *testing.T) {
			path, dir := fakeServer(t, mode)
			s, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			for range 2 {
				require.NoError(t, s.Send(t.Context(), domain.UserInput{Text: "hello"}))
				require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
				count := 1
				if mode == "early-events" {
					count = 100
				}
				for range count {
					require.Equal(t, domain.ModelEvent{Kind: domain.ModelEventText, Text: "hello"}, nextEvent(t, s))
				}
				call := nextEvent(t, s)
				require.Equal(t, domain.ModelEventToolCall, call.Kind)
				require.Equal(t, "mcp__noctiluca__reply", call.ToolCall.Name)
				require.JSONEq(t, `{"text":"hello"}`, string(call.ToolCall.Input))
				result := nextEvent(t, s)
				require.Equal(t, domain.ModelEventToolResult, result.Kind)
				require.Equal(t, call.ToolCall.ID, result.ToolCall.ID)
				require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
				require.True(t, s.Idle())
			}
		})
	}
}

func TestTurnTimeoutInterruptsBeforeProcessExit(t *testing.T) {
	for _, mode := range []string{"stay", "interrupt-blocked", "response-only"} {
		t.Run(mode, func(t *testing.T) {
			path, dir := fakeServer(t, mode)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s, err := New(path, "", "").Open(ctx, domain.ModelSessionConfig{Workdir: dir})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			require.NoError(t, s.Send(t.Context(), domain.UserInput{Text: "work"}))
			if mode != "response-only" {
				require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
			}
			require.False(t, s.Idle())
			cancel()
			closed := make(chan error, 1)
			go func() { closed <- s.Close() }()
			select {
			case err := <-closed:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("Close blocked after cancellation")
			}
			frames := requests(t, dir)
			last := frames[len(frames)-1]
			require.Equal(t, "turn/interrupt", last.Method)
			require.JSONEq(t, `{"threadId":"thread-1","turnId":"turn-1"}`, string(last.Params))
			require.Error(t, s.Send(t.Context(), domain.UserInput{Text: "closed"}))
		})
	}
}

func TestOpenFailures(t *testing.T) {
	for _, mode := range []string{"open-error", "malformed", "init-exit", "init-blocked"} {
		t.Run(mode, func(t *testing.T) {
			path, dir := fakeServer(t, mode)
			ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancel()
			_, err := New(path, "", "").Open(ctx, domain.ModelSessionConfig{Workdir: dir, ResumeToken: "saved",
				ToolHub: domain.MCPEndpoint{BearerToken: "secret-bearer"}})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret-bearer")
			for _, f := range requests(t, dir) {
				require.NotEqual(t, "thread/start", f.Method, "only a missing rollout warrants fallback")
			}
		})
	}
}

func TestTurnErrors(t *testing.T) {
	for _, mode := range []string{"reject", "rate-limit", "crash", "malformed-turn"} {
		t.Run(mode, func(t *testing.T) {
			path, dir := fakeServer(t, mode)
			s, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir, ToolHub: domain.MCPEndpoint{BearerToken: "secret-bearer"}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			err = s.Send(t.Context(), domain.UserInput{Text: ""})
			require.NoError(t, err)
			if mode == "reject" {
				event := nextEvent(t, s)
				require.Equal(t, domain.ModelEventError, event.Kind)
				require.ErrorContains(t, event.Err, "turn rejected")
				require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
				require.True(t, s.Idle())
				return
			}
			require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
			event := nextEvent(t, s)
			require.Equal(t, domain.ModelEventError, event.Kind)
			require.Error(t, event.Err)
			require.NotContains(t, event.Err.Error(), "secret-bearer")
			if mode == "rate-limit" {
				require.EqualError(t, event.Err, "quota [redacted] exhausted")
				require.Equal(t, domain.ModelEventError, nextEvent(t, s).Kind)
				require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
				require.True(t, s.Idle())
			}
		})
	}
}

func TestCloseKillsProcessGroup(t *testing.T) {
	path, dir := fakeServer(t, "descendant")
	s, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir})
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(data))
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() { require.NoError(t, s.Close()) })
	}
	wg.Wait()
	require.Eventually(t, func() bool {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		return strings.Contains(string(stat), ") Z ")
	}, time.Second, 10*time.Millisecond, "descendant survived Close")
}
