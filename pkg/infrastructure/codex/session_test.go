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
	capture, _ := json.Marshal(map[string]any{"args": args})
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
	const compactID = "compact-1"
	compactTurnNote := func(method, status string) {
		notify(method, map[string]any{"threadId": "thread-1", "turn": map[string]string{"id": compactID, "status": status}})
	}
	handleInput := inputTestServer(mode, emit, notify)
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
		if handleInput(f) {
			continue
		}
		switch f.Method {
		case "initialize":
			if mode == "init-exit" {
				os.Exit(0)
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
		case "thread/compact/start":
			if mode == "input-compact-reject" {
				reject("private diagnostic")
				continue
			}
			respond(map[string]any{})
			compactTurnNote("turn/started", "inProgress")
			if mode == "input-compact-crash" {
				os.Exit(1)
			}
			notify("item/agentMessage/delta", map[string]string{"threadId": "thread-1", "turnId": compactID, "delta": "internal text"})
		case "test/compact-complete":
			status := "completed"
			if mode == "input-compact-fail" {
				status = "failed"
				notify("error", map[string]any{"threadId": "thread-1", "turnId": compactID, "willRetry": false,
					"error": map[string]string{"message": "private diagnostic"}})
			} else {
				notify("thread/tokenUsage/updated", tokenUsageNote("thread-1", compactID, 80))
			}
			compactTurnNote("turn/completed", status)
			respond(map[string]any{})
		case "turn/start":
			if mode == "reject" {
				reject("turn rejected")
				continue
			}
			respond(map[string]any{"turn": map[string]string{"id": "turn-1"}})
			if mode == "response-only" {
				notify("item/agentMessage/delta", map[string]string{"threadId": "thread-1", "delta": "response sent"})
				continue
			}
			turnNote("turn/started", "inProgress")
			if mode == "interrupt-blocked" {
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
			notify("item/agentMessage/delta", map[string]string{"threadId": "thread-1", "delta": "hello"})
			for _, method := range []string{"item/started", "item/completed"} {
				notify(method, map[string]any{"threadId": "thread-1", "turnId": "turn-1",
					"item": map[string]any{"type": "mcpToolCall", "id": "tool-1", "server": "noctiluca", "tool": "reply", "arguments": map[string]string{"text": "hello"}, "status": "completed"}})
			}
			turnNote("turn/completed", "completed")
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

func tokenUsageNote(threadID, turnID string, tokens int) map[string]any {
	return map[string]any{"threadId": threadID, "turnId": turnID, "tokenUsage": map[string]any{
		"last": map[string]int{"totalTokens": tokens}, "total": map[string]int{"totalTokens": 987654},
	}}
}

func sendTokenUsage(t *testing.T, s *session, threadID string, tokens int) {
	t.Helper()
	params, err := json.Marshal(tokenUsageNote(threadID, "turn-1", tokens))
	require.NoError(t, err)
	control(t, s, "notify", rpcFrame{Method: "thread/tokenUsage/updated", Params: params})
}

func TestSessionContextTokens(t *testing.T) {
	s, _ := openInputSession(t, "input-normal")
	require.Zero(t, s.ContextTokens())
	sendTokenUsage(t, s, "thread-1", 234567)
	sendTokenUsage(t, s, "other-thread", 345678)
	sendTokenUsage(t, s, "", 456789)
	require.Equal(t, 234567, s.ContextTokens())
	require.Empty(t, s.events)
}

func waitForCompactionTurn(t *testing.T, s *session) {
	t.Helper()
	require.Eventually(t, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.current != nil && s.current.id == "compact-1"
	}, 5*time.Second, time.Millisecond)
	require.False(t, s.Idle())
}

func requireCompactionBlocked(t *testing.T, s *session) {
	t.Helper()
	require.Never(t, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.current != nil && s.current.compaction != nil
	}, 50*time.Millisecond, time.Millisecond, "compaction must wait for the session to become idle")
}

func TestSessionRequestedCompaction(t *testing.T) {
	path, dir := fakeServer(t, "input-normal")
	finished := make(chan struct{})
	model, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{
		Workdir: dir, OnCompactionDone: func() { close(finished) },
	})
	require.NoError(t, err)
	s := model.(*session)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	sendAndStart(t, s, "first turn")
	sendTokenUsage(t, s, "thread-1", 200000)
	s.Compact()
	s.Compact()
	requireCompactionBlocked(t, s)
	control(t, s, "complete", nil)
	require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
	waitForCompactionTurn(t, s)
	require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 2, Text: "after compaction"}))
	control(t, s, "compact-complete", nil)
	select {
	case <-finished:
	default:
		t.Fatal("compaction completion did not wake the caller")
	}
	requireEvents(t, s, domain.ModelEvent{Kind: domain.ModelEventTurnStarted},
		domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: 2},
		domain.ModelEvent{Kind: domain.ModelEventText, Text: "turn/start:after compaction"})
	require.Equal(t, 80, s.ContextTokens())
	control(t, s, "complete", nil)
	require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
	require.Eventually(t, s.Idle, time.Second, time.Millisecond)
	require.NoError(t, s.Close())
	require.Empty(t, s.events)
	var methods []string
	for _, frame := range requests(t, dir) {
		switch frame.Method {
		case "thread/compact/start":
			require.JSONEq(t, `{"threadId":"thread-1"}`, string(frame.Params))
			methods = append(methods, frame.Method)
		case "turn/start", "turn/steer", "test/complete", "test/compact-complete":
			methods = append(methods, frame.Method)
		}
	}
	require.Equal(t, []string{"turn/start", "test/complete", "thread/compact/start", "test/compact-complete", "turn/start", "test/complete"}, methods)
	var record struct {
		Level      string `json:"level"`
		PreTokens  int    `json:"pre_tokens"`
		PostTokens int    `json:"post_tokens"`
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record), "one log per compaction")
	require.Equal(t, "INFO", record.Level)
	require.Equal(t, 200000, record.PreTokens)
	require.Equal(t, 80, record.PostTokens)
}

func TestSessionSubagentCompletionDuringCompaction(t *testing.T) {
	s, _ := openInputSession(t, "input-normal")
	s.Compact()
	waitForCompactionTurn(t, s)
	control(t, s, "notify", rpcFrame{Method: "item/completed", Params: json.RawMessage(
		`{"threadId":"thread-1","turnId":"compact-1","item":{"type":"subAgentActivity","kind":"completed","agentThreadId":"child-1"}}`)})
	require.Equal(t, domain.ModelEventBackgroundDone, nextEvent(t, s).Kind)
	control(t, s, "compact-complete", nil)
	require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
	require.Equal(t, "turn/start:A subagent finished. Its result is available.", nextEvent(t, s).Text)
	control(t, s, "complete", nil)
	require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
	require.Eventually(t, s.Idle, time.Second, time.Millisecond)
	require.Empty(t, s.events, "an internal wake input has no acknowledgement")
}

func TestSessionCompactionWaitsForCommand(t *testing.T) {
	s, dir := openInputSession(t, "input-normal")
	sendAndStart(t, s, "run command")
	require.Equal(t, domain.ModelEventToolCall, commandNote(t, s, "item/started", "turn-1", "command", "inProgress", "").Kind)
	control(t, s, "complete", nil)
	require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
	s.Compact()
	requireCompactionBlocked(t, s)
	require.Equal(t, domain.ModelEventBackgroundDone, commandNote(t, s, "item/completed", "turn-1", "command", "completed", "").Kind)
	waitForCompactionTurn(t, s)
	control(t, s, "compact-complete", nil)
	require.Eventually(t, s.Idle, time.Second, time.Millisecond)
	require.NoError(t, s.Close())
	frames := requests(t, dir)
	require.Equal(t, "test/notify", frames[len(frames)-3].Method, "command completion precedes compaction")
	require.Equal(t, "thread/compact/start", frames[len(frames)-2].Method)
}

func TestSessionCompactionFailure(t *testing.T) {
	for _, mode := range []string{"input-compact-reject", "input-compact-fail", "input-compact-crash"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := openInputSession(t, mode)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			s.Compact()
			if mode == "input-compact-crash" {
				event := nextEvent(t, s)
				require.Equal(t, domain.ModelEventError, event.Kind)
				require.ErrorContains(t, event.Err, "codex process exited unexpectedly")
			} else {
				if mode == "input-compact-fail" {
					waitForCompactionTurn(t, s)
					require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 1, Text: "after failure"}))
					control(t, s, "compact-complete", nil)
				} else {
					require.Eventually(t, func() bool {
						s.mu.RLock()
						defer s.mu.RUnlock()
						return !s.compactRequested && s.idle()
					}, 5*time.Second, time.Millisecond)
					require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 1, Text: "after failure"}))
				}
				requireEvents(t, s, domain.ModelEvent{Kind: domain.ModelEventTurnStarted},
					domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: 1},
					domain.ModelEvent{Kind: domain.ModelEventText, Text: "turn/start:after failure"})
				control(t, s, "complete", nil)
				require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
			}
			require.NoError(t, s.Close())
			require.Empty(t, s.events)
			var record struct{ Level string }
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record), "one warning per failure")
			require.Equal(t, "WARN", record.Level)
			require.NotContains(t, logs.String(), "private diagnostic")
		})
	}
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

func nextTurnEvent(t *testing.T, s domain.ModelSession) domain.ModelEvent {
	t.Helper()
	for {
		event := nextEvent(t, s)
		if event.Kind != domain.ModelEventInputStarted {
			return event
		}
	}
}

func TestOpenResumeAndSettings(t *testing.T) {
	mainSessionTools := []string{"compact", "entry_send", "rest", "wake_at", "watch"}
	wantPrompt := "System instructions\n\n" +
		"In this harness, a command you leave running in the background never reports back to you. Run any command that may outlast the turn with the watch tool, never with nohup or &.\n" +
		"This main session delegates long or parallel work to subagents with spawn_agent. These instructions authorize it. Never call wait_agent. " +
		"Each brief must say that the subagent never posts and never calls these noctiluca tools: compact, entry_send, rest, wake_at, watch. " +
		"Continue a subagent with send_input. Close each finished agent."
	token := "thread-1:" + promptHash(wantPrompt)
	for _, resume := range []string{"", token} {
		t.Run("resume="+resume, func(t *testing.T) {
			path, dir := fakeServer(t, "normal")
			s, err := New(path, "test-model", "low").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir,
				ResumeToken: resume, SystemPrompt: "System instructions", ToolHub: domain.MCPEndpoint{
					URL: "http://127.0.0.1:1234/mcp", BearerToken: "secret-bearer", MainSessionTools: mainSessionTools,
				}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			require.True(t, s.Idle())
			require.Equal(t, token, s.ResumeToken())
			capture, err := os.ReadFile(filepath.Join(dir, "invocation.json"))
			require.NoError(t, err)
			var inv struct {
				Args []string `json:"args"`
			}
			require.NoError(t, json.Unmarshal(capture, &inv))
			require.Equal(t, []string{"app-server", "-c", "mcp_servers={}", "-c", "plugins={}"}, inv.Args)
			require.NotContains(t, string(capture), "secret-bearer")
			frames := requests(t, dir)
			require.Len(t, frames, 3)
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
			require.Equal(t, wantPrompt, params.DeveloperInstructions)
			require.Equal(t, "test-model", params.Model)
			require.Equal(t, map[string]any{"features.apps": false, "model_reasoning_effort": "low", "mcp_servers.noctiluca": map[string]any{
				"url": "http://127.0.0.1:1234/mcp", "http_headers": map[string]any{"Authorization": "Bearer secret-bearer"}}}, params.Config)
		})
	}
}

func TestMissingThreadFallback(t *testing.T) {
	path, dir := fakeServer(t, "missing")
	s, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir, ResumeToken: "missing"})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.Equal(t, "thread-1:"+promptHash(developerInstructions("", nil)), s.ResumeToken())
	frames := requests(t, dir)
	require.Len(t, frames, 4)
	require.Equal(t, "thread/resume", frames[2].Method)
	require.Equal(t, "thread/start", frames[3].Method)
	var params map[string]any
	require.NoError(t, json.Unmarshal(frames[3].Params, &params))
	require.NotContains(t, params, "threadId")
	require.NotContains(t, params, "excludeTurns")
}

func TestTurnStream(t *testing.T) {
	path, dir := fakeServer(t, "normal")
	s, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.Send(t.Context(), domain.UserInput{Text: "hello"}))
	require.Equal(t, domain.ModelEventTurnStarted, nextTurnEvent(t, s).Kind)
	require.Equal(t, domain.ModelEvent{Kind: domain.ModelEventText, Text: "hello"}, nextTurnEvent(t, s))
	call := nextTurnEvent(t, s)
	require.Equal(t, domain.ModelEventToolCall, call.Kind)
	require.Equal(t, "mcp__noctiluca__reply", call.ToolCall.Name)
	require.JSONEq(t, `{"text":"hello"}`, string(call.ToolCall.Input))
	result := nextTurnEvent(t, s)
	require.Equal(t, domain.ModelEventToolResult, result.Kind)
	require.Equal(t, call.ToolCall.ID, result.ToolCall.ID)
	require.Equal(t, domain.ModelEventTurnFinished, nextTurnEvent(t, s).Kind)
	require.Eventually(t, s.Idle, time.Second, time.Millisecond)
}

func TestCancellationInterruptsBeforeProcessExit(t *testing.T) {
	for _, mode := range []string{"interrupt-blocked", "response-only"} {
		t.Run(mode, func(t *testing.T) {
			path, dir := fakeServer(t, mode)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s, err := New(path, "", "").Open(ctx, domain.ModelSessionConfig{Workdir: dir})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			require.NoError(t, s.Send(t.Context(), domain.UserInput{Text: "work"}))
			if mode != "response-only" {
				require.Equal(t, domain.ModelEventTurnStarted, nextTurnEvent(t, s).Kind)
			} else {
				require.Equal(t, "response sent", nextTurnEvent(t, s).Text)
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
	for _, mode := range []string{"open-error", "init-exit"} {
		t.Run(mode, func(t *testing.T) {
			path, dir := fakeServer(t, mode)
			_, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir, ResumeToken: "saved",
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
	for _, mode := range []string{"rate-limit", "crash", "malformed-turn"} {
		t.Run(mode, func(t *testing.T) {
			path, dir := fakeServer(t, mode)
			s, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir, ToolHub: domain.MCPEndpoint{BearerToken: "secret-bearer"}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			err = s.Send(t.Context(), domain.UserInput{Text: ""})
			require.NoError(t, err)
			require.Equal(t, domain.ModelEventTurnStarted, nextTurnEvent(t, s).Kind)
			event := nextTurnEvent(t, s)
			require.Equal(t, domain.ModelEventError, event.Kind)
			require.Error(t, event.Err)
			require.NotContains(t, event.Err.Error(), "secret-bearer")
			if mode == "rate-limit" {
				require.EqualError(t, event.Err, "quota [redacted] exhausted")
				require.Equal(t, domain.ModelEventError, nextTurnEvent(t, s).Kind)
				require.Equal(t, domain.ModelEventTurnFinished, nextTurnEvent(t, s).Kind)
				require.Eventually(t, s.Idle, time.Second, time.Millisecond)
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
	require.NoError(t, s.Close())
	require.Eventually(t, func() bool {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		return strings.Contains(string(stat), ") Z ")
	}, time.Second, 10*time.Millisecond, "descendant survived Close")
}
