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
	"testing/synctest"
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
		fmt.Println(MinimumVersion + " (Claude Code)")
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
	finishCompact := func(id string) {
		if mode == "compact-failed" {
			emit(`{"type":"system","subtype":"status","status":null,"compact_result":"failed","compact_error":"private diagnostic"}`)
		} else {
			emit(`{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"manual","pre_tokens":200000,"post_tokens":12345}}`)
			emit(`{"type":"user","uuid":"summary","isCompactSummary":true,"message":{"content":"summary"}}`)
			emit(fmt.Sprintf(`{"type":"user","uuid":%q,"isReplay":true,"message":{"content":"/compact"}}`, id))
			emit(`{"type":"user","uuid":"history","isReplay":true,"message":{"content":[{"type":"tool_result","is_error":true,"content":"old tool error"}]}}`)
		}
		emit(`{"type":"result","subtype":"success","result":"","num_turns":0,"local_command":"compact"}`)
		emit(fmt.Sprintf(`{"type":"command_lifecycle","command_uuid":%q,"state":"completed"}`, id))
	}
	var compactID string
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
			var id string
			_ = json.Unmarshal(f["uuid"], &id)
			lifecycle := func(id, state string) {
				emit(fmt.Sprintf(`{"type":"command_lifecycle","command_uuid":%q,"state":%q}`, id, state))
			}
			if mode == "idle" {
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
					lifecycle(id, "started")
					emit(`{"type":"system","subtype":"task_notification","task_id":"task","status":"completed"}`)
					emit(`{"type":"result","subtype":"success"}`)
					lifecycle(id, "completed")
				}
				emit(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"checkpoint"}}}`)
				continue
			}
			_ = os.WriteFile("input-"+strconv.Itoa(count)+".json", scan.Bytes(), 0o600)
			if strings.HasPrefix(mode, "compact") {
				var msg struct{ Content string }
				_ = json.Unmarshal(f["message"], &msg)
				if msg.Content == "/compact" {
					if mode == "compact-task-turn" {
						lifecycle(id, "queued")
						emit(`{"type":"system","subtype":"task_notification","task_id":"background","status":"completed","summary":"done"}`)
						emit(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"task-turn","usage":{"input_tokens":42}}}}`)
						emit(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"background text"}}}`)
						emit(`{"type":"assistant","message":{"id":"task-turn","content":[{"type":"tool_use","id":"background-tool","name":"reply","input":{}}]}}`)
						emit(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"background-tool","is_error":true,"content":"background tool failed"}]}}`)
						emit(`{"type":"result","subtype":"success","result":"background result"}`)
						compactID = id
						continue
					}
					lifecycle(id, "started")
					emit(`{"type":"system","subtype":"status","status":"compacting"}`)
					if mode == "compact-held" {
						compactID = id
					} else {
						finishCompact(id)
					}
					continue
				}
				if compactID != "" {
					lifecycle(id, "queued")
					if mode == "compact-task-turn" {
						lifecycle(compactID, "started")
						emit(`{"type":"system","subtype":"status","status":"compacting"}`)
					}
					finishCompact(compactID)
					compactID = ""
				}
				if msg.Content == "pending" {
					lifecycle(id, "queued")
					emit(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"queued"}}}`)
					continue
				}
				lifecycle(id, "started")
				u := `{"input_tokens":10000,"cache_creation_input_tokens":10000,"cache_read_input_tokens":180000,"cache_creation":{"ephemeral_1h_input_tokens":10000,"ephemeral_5m_input_tokens":0}}`
				switch msg.Content {
				case "small", "subagent", "aggregate":
					u = `{"input_tokens":9999,"cache_creation_input_tokens":10000,"cache_read_input_tokens":180000,"cache_creation":{"ephemeral_1h_input_tokens":10000}}`
				case "five-minute":
					u = `{"input_tokens":10000,"cache_creation_input_tokens":10000,"cache_read_input_tokens":180000,"cache_creation":{"ephemeral_5m_input_tokens":10000}}`
				case "read-only":
					u = `{"input_tokens":20000,"cache_read_input_tokens":180000}`
				}
				emit(fmt.Sprintf(`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"request-%d","usage":%s}}}`, count, u))
				emit(fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"request-%d","content":[],"usage":%s}}`, count, u))
				if msg.Content == "subagent" || msg.Content == "child-five-minute" {
					childUsage := `{"input_tokens":999999,"cache_creation_input_tokens":10000,"cache_creation":{"ephemeral_5m_input_tokens":10000}}`
					emit(fmt.Sprintf(`{"type":"stream_event","parent_tool_use_id":"child","event":{"type":"message_start","message":{"id":"child","usage":%s}}}`, childUsage))
					emit(fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":"child","message":{"id":"child","content":[],"usage":%s}}`, childUsage))
				}
				if msg.Content == "active" {
					emit(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"active"}}}`)
					continue
				}
				if msg.Content == "background" {
					emit(`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"background"}]}`)
				}
				emit(`{"type":"result","subtype":"success","result":"reply","usage":{"input_tokens":999999,"cache_creation_input_tokens":999999}}`)
				lifecycle(id, "completed")
				continue
			}
			switch mode {
			case "malformed":
				emit("not-json")
				continue
			case "oversized":
				emit(strings.Repeat("x", maxFrameSize))
				continue
			case "orphan":
				os.Exit(3)
			case "flood":
				for i := 0; i < 1000; i++ {
					emit(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}}`)
				}
			case "permission":
				emit(`{"type":"control_request","request_id":"permission","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"anything"}}}`)
				continue
			}
			lifecycle(id, "started")
			emit(`{"type":"user","isReplay":true,"message":{"role":"user","content":"replayed input"}}`)
			// Exercise a UTF-8 frame split across writes.
			fmt.Print(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"`)
			emit(`日本語"}}}`)
			emit(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-1","name":"reply","input":{"text":"ok"}}]}}`)
			emit(`{"type":"result","subtype":"success","session_id":"test-session"}`)
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

func compactSession(t *testing.T, mode string) (*session, string) {
	t.Helper()
	path, dir := fakeCLI(t, mode)
	opened, err := New(path, "haiku").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir})
	require.NoError(t, err)
	s := opened.(*session)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	s.mu.Lock()
	s.compactDelay = 100 * time.Millisecond
	s.mu.Unlock()
	return s, dir
}

func compactTurn(t *testing.T, s *session, id int64, text string) {
	t.Helper()
	require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: id, Text: text}))
	require.Equal(t, domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}, nextEvent(t, s))
	require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
	require.Equal(t, domain.ModelEvent{Kind: domain.ModelEventTurnFinished, Text: "reply"}, nextEvent(t, s))
}

func awaitCompactTimer(t *testing.T, s *session) {
	t.Helper()
	require.Eventually(t, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.compactAt.IsZero() && s.parser.compactUUID == ""
	}, 5*time.Second, time.Millisecond)
}

func capturedInputs(t *testing.T, dir string) []frame {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "input-*.json"))
	require.NoError(t, err)
	inputs := make([]frame, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		f, err := parseFrame(data)
		require.NoError(t, err)
		inputs = append(inputs, f)
	}
	return inputs
}

func TestSessionIdleCompaction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inputs []string
	}{
		{"threshold", []string{"large"}},
		{"cache-read-keeps-last-write", []string{"large", "read-only"}},
		{"child-ttl-ignored", []string{"child-five-minute"}},
		{"exactly-sixty-seconds-late", []string{"large"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := compactSession(t, "compact")
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			for i, text := range tc.inputs {
				compactTurn(t, s, int64(i+1), text)
			}
			if tc.name == "exactly-sixty-seconds-late" {
				s.mu.Lock()
				now := s.compactAt.Add(idleCompactMaxLateness)
				s.now = func() time.Time { return now }
				s.mu.Unlock()
			}
			awaitCompactTimer(t, s)
			require.True(t, s.Idle())
			require.NoError(t, s.Close())
			require.Empty(t, s.events, "internal compaction must emit no core events")
			inputs := capturedInputs(t, dir)
			require.Len(t, inputs, len(tc.inputs)+1)
			last := inputs[len(inputs)-1]
			require.JSONEq(t, `"/compact"`, string(last.Message.Content))
			require.NotEmpty(t, last.UUID)
			require.NotEqual(t, inputs[0].UUID, last.UUID)
			require.Empty(t, last.ParentToolUseID)
			require.Equal(t, "test-session", last.SessionID)
			var record struct {
				Level      string `json:"level"`
				PreTokens  int    `json:"pre_tokens"`
				PostTokens int    `json:"post_tokens"`
			}
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record), "one log per attempt")
			require.Equal(t, "INFO", record.Level)
			require.Equal(t, 200000, record.PreTokens)
			require.Equal(t, 12345, record.PostTokens)
		})
	}
}

func TestSessionIdleCompactionSkipped(t *testing.T) {
	for _, name := range []string{"small", "five-minute", "active", "background", "pending", "late", "read-only", "subagent", "aggregate", "last-write-five-minute"} {
		t.Run(name, func(t *testing.T) {
			s, dir := compactSession(t, "compact")
			text := name
			if name == "pending" || name == "late" || name == "last-write-five-minute" {
				text = "large"
			}
			if name == "active" {
				require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 1, Text: text}))
				require.Equal(t, domain.ModelEventInputStarted, nextEvent(t, s).Kind)
				require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
				require.Equal(t, "active", nextEvent(t, s).Text)
			} else {
				compactTurn(t, s, 1, text)
			}
			inputCount := 1
			if name == "pending" {
				require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 2, Text: "pending"}))
				require.Equal(t, "queued", nextEvent(t, s).Text)
				inputCount++
			}
			if name == "last-write-five-minute" {
				compactTurn(t, s, 2, "five-minute")
				inputCount++
			}
			if name == "late" {
				s.mu.Lock()
				now := s.compactAt.Add(idleCompactMaxLateness + time.Nanosecond)
				s.now = func() time.Time { return now }
				s.mu.Unlock()
			}
			awaitCompactTimer(t, s)
			require.NoError(t, s.Close())
			require.Empty(t, s.events)
			require.Len(t, capturedInputs(t, dir), inputCount, "the CLI must receive no /compact")
		})
	}
}

func TestSessionInputDuringCompaction(t *testing.T) {
	s, dir := compactSession(t, "compact-held")
	compactTurn(t, s, 1, "large")
	require.Eventually(t, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.parser.compacting
	}, 5*time.Second, time.Millisecond)
	require.False(t, s.Idle())
	s.mu.RLock()
	require.Empty(t, s.pending, "compaction must not enter the pending input map")
	s.mu.RUnlock()
	compactTurn(t, s, 2, "small")
	require.True(t, s.Idle())
	require.NoError(t, s.Close())
	require.Empty(t, s.events)
	inputs := capturedInputs(t, dir)
	require.Len(t, inputs, 3)
	require.JSONEq(t, `"/compact"`, string(inputs[1].Message.Content))
	require.JSONEq(t, `"small"`, string(inputs[2].Message.Content))
}

func TestSessionAutonomousTurnBeforeCompaction(t *testing.T) {
	s, dir := compactSession(t, "compact-task-turn")
	compactTurn(t, s, 1, "large")
	require.Equal(t, domain.ModelEventBackgroundDone, nextEvent(t, s).Kind)
	require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
	require.Equal(t, domain.ModelEvent{Kind: domain.ModelEventText, Text: "background text"}, nextEvent(t, s))
	tool := nextEvent(t, s)
	require.Equal(t, domain.ModelEventToolCall, tool.Kind)
	require.Equal(t, "background-tool", tool.ToolCall.ID)
	result := nextEvent(t, s)
	require.Equal(t, domain.ModelEventToolResult, result.Kind)
	require.EqualError(t, result.Err, "background tool failed")
	require.Equal(t, domain.ModelEvent{Kind: domain.ModelEventTurnFinished, Text: "background result"}, nextEvent(t, s))
	require.False(t, s.Idle(), "queued compaction must keep the session busy")
	s.mu.RLock()
	compacting, compactUUID, contextTokens := s.parser.compacting, s.parser.compactUUID, s.contextTokens
	s.mu.RUnlock()
	require.False(t, compacting, "compaction must wait for its own started frame")
	require.NotEmpty(t, compactUUID)
	require.Equal(t, 42, contextTokens, "the autonomous turn must update the main-thread context")
	compactTurn(t, s, 2, "small")
	require.True(t, s.Idle())
	require.NoError(t, s.Close())
	require.Empty(t, s.events, "internal compaction must emit no core events")
	inputs := capturedInputs(t, dir)
	require.Len(t, inputs, 3)
	require.JSONEq(t, `"/compact"`, string(inputs[1].Message.Content))
}

func TestSessionIdleCompactionFailure(t *testing.T) {
	s, dir := compactSession(t, "compact-failed")
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	compactTurn(t, s, 1, "large")
	awaitCompactTimer(t, s)
	require.True(t, s.Idle())
	compactTurn(t, s, 2, "small")
	require.NoError(t, s.Close())
	require.Empty(t, s.events)
	require.Len(t, capturedInputs(t, dir), 3)
	var record struct{ Level string }
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record))
	require.Equal(t, "WARN", record.Level)
	require.NotContains(t, logs.String(), "private diagnostic")
}

func TestSessionIdleCompactionRearms(t *testing.T) {
	s, dir := compactSession(t, "compact")
	s.mu.Lock()
	s.compactDelay = 400 * time.Millisecond
	s.mu.Unlock()
	compactTurn(t, s, 1, "large")
	s.mu.RLock()
	firstDeadline := s.compactAt
	s.mu.RUnlock()
	<-time.NewTimer(time.Until(firstDeadline.Add(-200 * time.Millisecond))).C
	compactTurn(t, s, 2, "large")
	s.mu.RLock()
	secondDeadline := s.compactAt
	s.mu.RUnlock()
	require.True(t, secondDeadline.After(firstDeadline))
	<-time.NewTimer(time.Until(firstDeadline.Add(50 * time.Millisecond))).C
	require.Len(t, capturedInputs(t, dir), 2, "the superseded timer must not compact")
	awaitCompactTimer(t, s)
	require.NoError(t, s.Close())
	require.Empty(t, s.events)
	require.Len(t, capturedInputs(t, dir), 3)
}

func TestSessionCloseStopsCompaction(t *testing.T) {
	for _, mode := range []string{"compact", "compact-held"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := compactSession(t, mode)
			compactTurn(t, s, 1, "large")
			if mode == "compact-held" {
				require.Eventually(t, func() bool { return !s.Idle() }, 5*time.Second, time.Millisecond)
			}
			require.NoError(t, s.Close())
			select {
			case <-s.compactDone:
			default:
				t.Fatal("compaction goroutine outlived the session")
			}
		})
	}
}

func TestCompactionWaitsForInputWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		input, output, err := os.Pipe()
		require.NoError(t, err)
		defer input.Close()
		defer output.Close()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s := &session{
			ctx: ctx, cancel: cancel, stdin: output, writeLock: make(chan struct{}, 1),
			compactAt: time.Now(), now: time.Now, contextTokens: idleCompactMinTokens, oneHourCache: true,
		}
		s.writeLock <- struct{}{}
		compacted := make(chan struct{})
		go func() { s.compactIfIdle(); close(compacted) }()
		synctest.Wait()
		require.True(t, s.Idle(), "compaction must reserve stdin before it marks the session busy")
		sent := make(chan error, 1)
		go func() { sent <- s.Send(ctx, domain.UserInput{ID: 1, Text: "core input"}) }()
		synctest.Wait()
		require.False(t, s.Idle())
		<-s.writeLock
		synctest.Wait()
		<-compacted
		require.NoError(t, <-sent)
		require.NoError(t, output.Close())
		scan := bufio.NewScanner(input)
		require.True(t, scan.Scan())
		f, err := parseFrame(scan.Bytes())
		require.NoError(t, err)
		require.JSONEq(t, `"core input"`, string(f.Message.Content))
		require.False(t, scan.Scan(), "a queued core input must prevent compaction")
		require.NoError(t, scan.Err())
	})
}

func TestMainRequestTracking(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	s := &session{compactWake: make(chan struct{}, 1), compactDelay: idleCompactDelay, now: func() time.Time { return now }}
	observe := func(line string) {
		t.Helper()
		f, err := parseFrame([]byte(line))
		require.NoError(t, err)
		s.observeRequest(f)
	}
	observe(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"first","usage":{"input_tokens":1,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"cache_creation":{"ephemeral_1h_input_tokens":2}}}}}`)
	firstDeadline := s.compactAt
	require.Equal(t, now.Add(54*time.Minute), firstDeadline)
	require.Equal(t, 6, s.contextTokens)
	require.True(t, s.oneHourCache)
	now = now.Add(time.Minute)
	observe(`{"type":"assistant","message":{"id":"first","content":[],"usage":{"input_tokens":2,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"cache_creation":{"ephemeral_1h_input_tokens":2}}}}`)
	require.Equal(t, firstDeadline, s.compactAt, "a final frame belongs to the same request as message_start")
	require.Equal(t, 7, s.contextTokens)
	observe(`{"type":"stream_event","parent_tool_use_id":"child","event":{"type":"message_start","message":{"id":"child","usage":{"input_tokens":999999}}}}`)
	require.Equal(t, firstDeadline, s.compactAt)
	require.Equal(t, 7, s.contextTokens)
	observe(`{"type":"assistant","message":{"id":"second","content":[],"usage":{"input_tokens":8}}}`)
	require.Equal(t, now.Add(54*time.Minute), s.compactAt, "assistant usage also identifies a new main-thread request")
	require.Equal(t, 8, s.contextTokens)
	require.True(t, s.oneHourCache, "a cache read does not replace the last cache write's TTL")
}

func TestVersionCompatibility(t *testing.T) {
	for _, tc := range []struct {
		version string
		invalid bool
	}{
		{"1.999.999", true},
		{"2.0.999", true},
		{"2.1.315", true},
		{"2.1.316", false},
		{"2.1.1000", false},
		{"3.0.0", false},
		{"2.1", true},
		{"2.1.-1", true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			err := checkVersion(t.Context(), tc.version+" (Claude Code)")
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
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

func TestSessionResumeAndConfig(t *testing.T) {
	path, dir := fakeCLI(t, "echo")
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
	require.Equal(t, "Owner instructions\n\n"+subagentInstruction, inv.Prompt)
	require.JSONEq(t, `{"mcpServers":{"noctiluca":{"type":"http","url":"http://127.0.0.1:1234/mcp","headers":{"Authorization":"Bearer test-bearer"}}}}`, string(inv.MCP))
	joined := strings.Join(inv.Args, " ")
	require.NotContains(t, joined, "--bare")
	require.NotContains(t, joined, "test-bearer")
	require.True(t, s.Idle())
	for _, flag := range []string{"--resume " + savedSessionID, "--model haiku", "--setting-sources user,project,local", "--permission-mode default", "--strict-mcp-config", "--allowedTools mcp__noctiluca__*"} {
		require.Contains(t, joined, flag)
	}
	require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 1, Text: "first"}))
	require.Equal(t, domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: 1}, nextEvent(t, s))
	require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
	require.Equal(t, "日本語", nextEvent(t, s).Text)
	for nextEvent(t, s).Kind != domain.ModelEventTurnFinished {
	}
	require.Equal(t, "test-session", s.ResumeToken())
	require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 2, Text: "second", Attachments: []domain.Attachment{{Ref: "file-ref", Name: "photo"}}}))
	for nextEvent(t, s).Kind != domain.ModelEventTurnFinished {
	}
	data, err := os.ReadFile(filepath.Join(dir, "input-2.json"))
	require.NoError(t, err)
	var input struct {
		SessionID string `json:"session_id"`
		Message   struct{ Content string }
	}
	require.NoError(t, json.Unmarshal(data, &input))
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
			invocations := readInvocations(t, dir)
			require.Len(t, invocations, 2)
			require.Contains(t, strings.Join(invocations[0].Args, " "), "--resume "+savedSessionID)
			require.NotContains(t, invocations[1].Args, "--resume")
			for _, inv := range invocations {
				require.Contains(t, strings.Join(inv.Args, " "), "--model haiku")
				require.Equal(t, systemPrompt(config.SystemPrompt), inv.Prompt)
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
			path, dir := fakeCLI(t, "init-exit")
			t.Setenv("NOCTILUCA_TEST_INIT_ERROR", tc.frame)
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
		})
	}
}

func TestMissingResumeFallbackFailure(t *testing.T) {
	detail := "No conversation found with session ID: " + savedSessionID
	path, dir := fakeCLI(t, "missing-resume")
	t.Setenv("NOCTILUCA_TEST_INIT_ERROR", fmt.Sprintf(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":[%q]}`, detail))
	s, err := New(path, "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir, ResumeToken: savedSessionID})
	require.Nil(t, s)
	require.ErrorContains(t, err, "claude exited before initialization: "+detail)
	invocations := readInvocations(t, dir)
	require.Len(t, invocations, 2)
	require.Contains(t, strings.Join(invocations[0].Args, " "), "--resume "+savedSessionID)
	require.NotContains(t, invocations[1].Args, "--resume")
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

func TestCompactionWriteFailure(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(fmt.Sprintf("closing=%t", closing), func(t *testing.T) {
			input, output, err := os.Pipe()
			require.NoError(t, err)
			require.NoError(t, input.Close())
			t.Cleanup(func() { require.NoError(t, output.Close()) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := &session{
				ctx: ctx, cancel: cancel, stdin: output, writeLock: make(chan struct{}, 1),
				compactAt: time.Now().Add(-time.Second), now: time.Now, contextTokens: idleCompactMinTokens, oneHourCache: true,
			}
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			if closing {
				cancel()
			}
			s.compactIfIdle()
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			if closing {
				require.Empty(t, logs.String())
				return
			}
			require.NotEmpty(t, logs.String(), "a failed internal write must log before the session closes")
			var record struct {
				Level string `json:"level"`
				Msg   string `json:"msg"`
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record), "one warning per failed write")
			require.Equal(t, "WARN", record.Level)
			require.Equal(t, "Write Claude idle compaction failed", record.Msg)
			require.Equal(t, "write claude input failed", record.Error)
		})
	}
}

func TestSessionFailures(t *testing.T) {
	for _, mode := range []string{"malformed", "oversized", "orphan", "permission"} {
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
				if mode == "orphan" {
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
		for _, kind := range []domain.ModelEventKind{domain.ModelEventInputStarted, domain.ModelEventTurnStarted, domain.ModelEventText, domain.ModelEventToolCall, domain.ModelEventTurnFinished} {
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
