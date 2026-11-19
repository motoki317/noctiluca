//go:build live

package codex

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/usecase/toolhub"
)

// This test spends one subscription turn and checks that Codex leaves config.toml unchanged.
func TestLiveHubTurn(t *testing.T) {
	workdir := liveWorkdir(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	hub, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	replies := make(chan string, 1)
	require.NoError(t, hub.AddSource("test", []domain.Tool{{Name: "reply", Description: "Send a reply to the current conversation.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`),
		Handler: func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
			var args struct{ Text string }
			if err := json.Unmarshal(input, &args); err != nil {
				return nil, err
			}
			select {
			case replies <- args.Text:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return json.RawMessage(`{"sent":true}`), nil
		}}}))
	hubDone := make(chan error, 1)
	go func() { hubDone <- hub.Start(ctx) }()
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		require.NoError(t, hub.Shutdown(stopCtx))
		require.NoError(t, <-hubDone)
	})
	endpoint, err := hub.OpenSession("live-session")
	require.NoError(t, err)
	s, err := New("", "gpt-6-luna", "low").Open(ctx, domain.ModelSessionConfig{Workdir: workdir, ToolHub: endpoint,
		SystemPrompt: "This is a smoke test. Send replies through the reply tool. Do not read or change files or use other tools."})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NotEmpty(t, s.ResumeToken())
	require.NoError(t, s.Send(ctx, domain.UserInput{Text: "Call the reply tool exactly once with text PONG, then finish."}))
	started, called := false, false
	for {
		select {
		case event, ok := <-s.Events():
			require.True(t, ok)
			require.NoError(t, event.Err)
			switch event.Kind {
			case domain.ModelEventTurnStarted:
				started = true
			case domain.ModelEventToolCall:
				called = event.ToolCall.Name == "mcp__noctiluca__reply" || called
			case domain.ModelEventTurnFinished:
				require.True(t, started)
				require.True(t, called)
				require.True(t, s.Idle())
				select {
				case reply := <-replies:
					require.Equal(t, "PONG", reply)
				default:
					t.Fatal("turn ended without a hub reply")
				}
				require.NoError(t, s.Close())
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestLiveCompaction(t *testing.T) {
	workdir := liveWorkdir(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	codex, err := exec.LookPath("codex")
	require.NoError(t, err)
	dir := t.TempDir()
	trace := filepath.Join(dir, "stdout.jsonl")
	t.Setenv("NOCTILUCA_TEST_CODEX", codex)
	t.Setenv("NOCTILUCA_TEST_TRACE", trace)
	wrapper := filepath.Join(dir, "codex")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\numask 077\n\"$NOCTILUCA_TEST_CODEX\" \"$@\" | tee \"$NOCTILUCA_TEST_TRACE\"\n"), 0700))
	model, err := New(wrapper, "gpt-6-luna", "low").Open(ctx, domain.ModelSessionConfig{Workdir: workdir,
		SystemPrompt: "This is a compaction smoke test. Reply directly. Do not use tools."})
	require.NoError(t, err)
	s := model.(*session)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.Send(ctx, domain.UserInput{ID: 1, Text: "Reply PONG without using tools, then finish."}))
	var finished, acknowledged bool
	for !finished || !acknowledged {
		select {
		case event, ok := <-s.Events():
			require.True(t, ok)
			require.NoError(t, event.Err)
			switch event.Kind {
			case domain.ModelEventTurnFinished:
				finished = true
			case domain.ModelEventInputStarted:
				acknowledged = true
			}
		case <-ctx.Done():
			t.Fatal("initial turn did not finish: ", ctx.Err())
		}
	}
	before := s.ContextTokens()
	require.Positive(t, before)
	liveCompact(t, s)
	after := s.ContextTokens()
	require.Positive(t, after)
	// A later response gives tee time to copy the preceding frames to the trace before shutdown.
	require.NoError(t, s.call(ctx, "thread/read", map[string]string{"threadId": s.threadID}, nil))
	require.NoError(t, s.Close())
	require.Empty(t, s.events, "compaction has no core events")

	file, err := os.Open(trace)
	require.NoError(t, err)
	defer file.Close()
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 64<<10), maxFrameSize)
	var turnIDs, steps []string
	var updatedTokens int
	for scan.Scan() {
		var f rpcFrame
		require.NoError(t, json.Unmarshal(scan.Bytes(), &f))
		if f.Method == "" || len(f.Params) == 0 {
			continue
		}
		var n notification
		require.NoError(t, json.Unmarshal(f.Params, &n))
		if n.ThreadID != s.threadID {
			continue
		}
		switch f.Method {
		case "turn/started":
			turnIDs = append(turnIDs, n.Turn.ID)
		case "turn/completed":
			require.NotEmpty(t, turnIDs)
			require.Equal(t, turnIDs[len(turnIDs)-1], n.Turn.ID)
			require.Equal(t, "completed", n.Turn.Status)
		case "item/started", "item/completed":
			var item struct{ Type string }
			require.NoError(t, json.Unmarshal(n.Item, &item))
			if item.Type != "contextCompaction" {
				continue
			}
			require.Len(t, turnIDs, 2)
			require.Equal(t, turnIDs[1], n.TurnID)
		case "thread/tokenUsage/updated":
			if len(turnIDs) < 2 {
				continue
			}
			require.Equal(t, turnIDs[1], n.TurnID)
			require.NotNil(t, n.TokenUsage.Last.TotalTokens)
			updatedTokens = *n.TokenUsage.Last.TotalTokens
		default:
			continue
		}
		steps = append(steps, f.Method)
	}
	require.NoError(t, scan.Err())
	require.Equal(t, []string{"turn/started", "turn/completed", "turn/started", "item/started",
		"thread/tokenUsage/updated", "item/completed", "turn/completed"}, steps)
	require.NotEqual(t, turnIDs[0], turnIDs[1])
	require.Equal(t, updatedTokens, after)
	t.Logf("notifications: %s", strings.Join(steps, " -> "))
	t.Logf("context tokens before and after compaction: %d -> %d", before, after)
}

func liveCompact(t *testing.T, s *session) {
	t.Helper()
	s.Compact()
	require.Eventually(t, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return !s.compactRequested && s.idle()
	}, 60*time.Second, 10*time.Millisecond, "the requested compaction must finish")
}

func TestLiveSubagentHubAndWakeup(t *testing.T) {
	workdir := liveWorkdir(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	hub, err := toolhub.New(toolhub.Config{})
	require.NoError(t, err)
	value := rand.Text()
	called, release := make(chan struct{}, 1), make(chan struct{})
	require.NoError(t, hub.AddSource("test", []domain.Tool{{Name: "read_probe", ReadOnly: true,
		Description: "Read the test's secret value. Only the delegated subagent calls this tool.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Handler: func(callCtx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			select {
			case called <- struct{}{}:
			default:
			}
			select {
			case <-release:
				return json.Marshal(map[string]string{"value": value})
			case <-callCtx.Done():
				return nil, callCtx.Err()
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}}))
	hubDone := make(chan error, 1)
	go func() { hubDone <- hub.Start(ctx) }()
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		require.NoError(t, hub.Shutdown(stopCtx))
		require.NoError(t, <-hubDone)
	})
	endpoint, err := hub.OpenSession("live-subagent")
	require.NoError(t, err)
	model, err := New("", "gpt-6-luna", "low").Open(ctx, domain.ModelSessionConfig{Workdir: workdir, ToolHub: endpoint,
		SystemPrompt: "This is a delegation test. Spawn one subagent, then end your turn immediately. Never wait for it, poll it, or close it before it finishes. Only the child calls read_probe. Do not read or change files, run commands, or call other MCP tools. If a later completion starts a new turn, report the child's result and close the finished agent."})
	require.NoError(t, err)
	s := model.(*session)
	var starts atomic.Int32
	var parentRead atomic.Bool
	finished, eventErrors, drained := make(chan struct{}, 1), make(chan error, 1), make(chan struct{})
	// Drain parent events while thread/read waits for its RPC response.
	go func() {
		defer close(drained)
		for event := range s.Events() {
			if event.Err != nil {
				select {
				case eventErrors <- event.Err:
				default:
				}
			}
			switch event.Kind {
			case domain.ModelEventTurnStarted:
				starts.Add(1)
			case domain.ModelEventToolCall:
				if event.ToolCall.Name == "mcp__noctiluca__read_probe" {
					parentRead.Store(true)
				}
			case domain.ModelEventTurnFinished:
				select {
				case finished <- struct{}{}:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() { require.NoError(t, s.Close()); <-drained })
	require.NoError(t, s.Send(ctx, domain.UserInput{Text: `Use spawn_agent to delegate this exact brief: "Objective: prove a subagent can read the noctiluca tool hub. Call mcp__noctiluca__read_probe exactly once with {} and return its value verbatim as your final answer. The subagent never posts or calls a messaging tool that writes to chat. Do not use files, commands, other tools, or further subagents. Finish after returning the value." Acknowledge that you delegated, then end this turn without waiting.`}))
	select {
	case <-finished:
	case err := <-eventErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("parent did not end its turn: ", ctx.Err())
	}
	require.EqualValues(t, 1, starts.Load())
	select {
	case <-called:
	case err := <-eventErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("subagent did not call read_probe: ", ctx.Err())
	}
	readThread := func(ctx context.Context, id string) liveThread {
		var result struct{ Thread liveThread }
		require.NoError(t, s.call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": true}, &result))
		return result.Thread
	}
	parent := readThread(ctx, s.threadID)
	var childID string
	for _, turn := range parent.Turns {
		for _, item := range turn.Items {
			if item.Type == "subAgentActivity" && item.Kind == "started" {
				require.Empty(t, childID, "expected exactly one subagent")
				childID = item.AgentThreadID
			}
		}
	}
	require.NotEmpty(t, childID, "parent history did not record a subagent")
	before := s.ContextTokens()
	liveCompact(t, s)
	parent = readThread(ctx, s.threadID)
	var compacted bool
	for _, turn := range parent.Turns {
		for _, item := range turn.Items {
			compacted = compacted || (turn.Status == "completed" && item.Type == "contextCompaction")
		}
	}
	require.True(t, compacted, "compaction must finish while the child's read is held")
	require.EqualValues(t, 1, starts.Load(), "compaction must not appear as a parent turn")
	t.Logf("parent compacted while the child was blocked in read_probe: %d -> %d tokens", before, s.ContextTokens())
	close(release)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var child liveThread
	for {
		child = readThread(ctx, childID)
		if len(child.Turns) > 0 {
			status := child.Turns[len(child.Turns)-1].Status
			require.NotContains(t, []string{"failed", "interrupted"}, status, "child turn failed")
			if status == "completed" {
				break
			}
		}
		select {
		case err := <-eventErrors:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("child did not finish: ", ctx.Err())
		case <-ticker.C:
		}
	}
	require.Equal(t, s.threadID, child.ParentThreadID)
	var read, answer bool
	for _, turn := range child.Turns {
		for _, item := range turn.Items {
			if item.Type == "mcpToolCall" && item.Server == "noctiluca" && item.Tool == "read_probe" && item.Status == "completed" {
				read = true
			}
			if item.Type == "agentMessage" && strings.Contains(item.Text, value) {
				answer = true
			}
		}
	}
	require.True(t, read, "child history must contain its own successful hub read")
	require.True(t, answer, "child must return the value that only the hub supplied")
	t.Log("subagent read noctiluca.read_probe and returned the probe value after compaction")
	const observationWindow = 15 * time.Second
	observation, cancelObservation := context.WithTimeout(ctx, observationWindow)
	defer cancelObservation()
	for starts.Load() == 1 || len(readThread(observation, s.threadID).Turns) <= len(parent.Turns) {
		select {
		case <-observation.Done():
			t.Fatal("subagent completion did not start a parent turn within ", observationWindow)
		case err := <-eventErrors:
			t.Fatal(err)
		case <-ticker.C:
		}
	}
	require.NoError(t, observation.Err(), "parent turn must start within the observation window")
	require.Greater(t, starts.Load(), int32(1), "subagent completion must start a parent turn")
	t.Logf("parent turn started within %s after child completion", observationWindow)
	select {
	case <-finished:
	case err := <-eventErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("parent did not finish its wake turn: ", ctx.Err())
	}
	woken := readThread(ctx, s.threadID)
	require.Greater(t, len(woken.Turns), len(parent.Turns))
	var parentAnswer bool
	for _, turn := range woken.Turns[len(parent.Turns):] {
		for _, item := range turn.Items {
			parentAnswer = parentAnswer || (item.Type == "agentMessage" && strings.Contains(item.Text, value))
		}
	}
	require.True(t, parentAnswer, "the woken parent must receive the subagent's probe value")
	t.Log("the woken parent reported the child's probe value")
	require.False(t, parentRead.Load(), "the parent must not substitute its own read for the child's")
}

type liveThread struct {
	ParentThreadID string
	Turns          []struct {
		Status string
		Items  []struct {
			Type, Kind, AgentThreadID, Server, Tool, Status, Text string
		}
	}
}

func liveWorkdir(t *testing.T) string {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Skip("live subscription test is disabled in CI")
	}
	workdir := os.Getenv("NOCTILUCA_LIVE_CODEX_WORKDIR")
	if workdir == "" {
		t.Skip("set NOCTILUCA_LIVE_CODEX_WORKDIR to an existing directory without a .codex/ directory")
	}
	_, err := os.Stat(filepath.Join(workdir, ".codex"))
	require.True(t, os.IsNotExist(err), "live workdir must not contain a .codex directory")
	configPath := liveConfigPath(t, workdir)
	before := readLiveConfig(t, configPath)
	t.Logf("config before: exists=%t sha256=%x", before.exists, before.hash)
	t.Cleanup(func() {
		after := readLiveConfig(t, configPath)
		t.Logf("config after: exists=%t sha256=%x", after.exists, after.hash)
		require.Equal(t, before, after, "Codex changed config.toml. The test fails without restoring the file so the changes remain available for inspection.")
	})
	return workdir
}

func liveConfigPath(t *testing.T, workdir string) string {
	t.Helper()
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		require.NoError(t, err)
		return filepath.Join(userHome, ".codex", "config.toml")
	}
	if !filepath.IsAbs(home) {
		workdir, err := filepath.Abs(workdir)
		require.NoError(t, err)
		// Keep '..' intact until symlinks resolve, as Codex does.
		home = workdir + string(filepath.Separator) + home
	}
	home, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	return filepath.Join(home, "config.toml")
}

type liveConfigState struct {
	exists bool
	hash   [sha256.Size]byte
}

func readLiveConfig(t *testing.T, path string) liveConfigState {
	t.Helper()
	data, err := os.ReadFile(path)
	if !os.IsNotExist(err) {
		require.NoError(t, err)
	}
	return liveConfigState{exists: err == nil, hash: sha256.Sum256(data)}
}
