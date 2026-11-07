// Codex app-server protocol (Apache-2.0): https://github.com/openai/codex.
// Independent Go implementation. See THIRD_PARTY_NOTICES.md.

package codex

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type adapter struct{ executable, model, effort string }

const backgroundInstruction = "In this harness, a command you leave running in the background never reports back to you. Run any command that may outlast the turn with the watch tool, never with nohup or &."

func developerInstructions(prompt string) string { return prompt + "\n\n" + backgroundInstruction }

func promptHash(prompt string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(prompt)))[:16] }

func formatResumeToken(threadID, hash string) string { return threadID + ":" + hash }

func parseResumeToken(token string) (string, string) {
	threadID, hash, _ := strings.Cut(token, ":")
	return threadID, hash
}

func New(executable, model, effort string) domain.ModelAdapter {
	if executable == "" {
		executable = "codex"
	}
	return &adapter{executable: executable, model: model, effort: effort}
}

func (a *adapter) Open(ctx context.Context, config domain.ModelSessionConfig) (domain.ModelSession, error) {
	workdir, err := filepath.Abs(config.Workdir)
	if err != nil {
		return nil, fmt.Errorf("resolve codex workdir: %w", err)
	}
	config.Workdir = workdir
	s, err := startSession(ctx, a.executable, config)
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			_ = s.Close()
		}
	}()
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.call(initCtx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "noctiluca", "version": "0.1.0"}}, nil); err != nil {
		return nil, fmt.Errorf("initialize codex: %w", err)
	}
	if err := s.write(initCtx, rpcFrame{Method: "initialized"}); err != nil {
		return nil, err
	}
	overrides := map[string]any{"features.apps": false}
	if a.effort != "" {
		overrides["model_reasoning_effort"] = a.effort
	}
	if config.ToolHub.URL != "" {
		overrides["mcp_servers.noctiluca"] = map[string]any{
			"url":          config.ToolHub.URL,
			"http_headers": map[string]string{"Authorization": "Bearer " + config.ToolHub.BearerToken},
		}
	}
	prompt := developerInstructions(config.SystemPrompt)
	threadID, previousHash := parseResumeToken(config.ResumeToken)
	params := threadParams{Cwd: workdir, Model: a.model, DeveloperInstructions: prompt,
		ApprovalPolicy: "never", Sandbox: "danger-full-access", Config: overrides, ThreadID: threadID}
	method := "thread/start"
	if params.ThreadID != "" {
		method = "thread/resume"
		params.ExcludeTurns = true
	}
	var result threadResult
	err = s.call(initCtx, method, params, &result)
	var rpcErr *rpcError
	// A deleted rollout file leaves no thread to resume.
	if params.ThreadID != "" && errors.As(err, &rpcErr) && rpcErr.Code == -32600 && strings.Contains(rpcErr.Message, "no rollout found") {
		slog.WarnContext(ctx, "Codex thread is unavailable; starting a fresh session")
		params.ThreadID, params.ExcludeTurns = "", false
		err = s.call(initCtx, "thread/start", params, &result)
	}
	if err != nil {
		return nil, fmt.Errorf("open codex thread: %w", err)
	}
	if result.Thread.ID == "" {
		return nil, errors.New("codex returned no thread id")
	}
	if params.ThreadID != "" && previousHash != promptHash(prompt) {
		err := s.call(initCtx, "thread/inject_items", map[string]any{
			"threadId": result.Thread.ID,
			"items": []map[string]any{{"type": "message", "role": "developer",
				"content": []map[string]string{{"type": "input_text", "text": prompt}}}},
		}, nil)
		// Older Codex versions lack thread/inject_items.
		if errors.As(err, &rpcErr) && (rpcErr.Code == -32601 ||
			(rpcErr.Code == -32600 && strings.HasPrefix(rpcErr.Message, "Invalid request: unknown variant `thread/inject_items`"))) {
			slog.WarnContext(ctx, "Codex prompt injection is unavailable; starting a fresh session")
			params.ThreadID, params.ExcludeTurns = "", false
			result = threadResult{}
			err = s.call(initCtx, "thread/start", params, &result)
		}
		if err != nil {
			return nil, fmt.Errorf("refresh codex instructions: %w", err)
		}
		if result.Thread.ID == "" {
			return nil, errors.New("codex returned no thread id")
		}
	}
	s.mu.Lock()
	s.threadID, s.promptHash = result.Thread.ID, promptHash(prompt)
	s.mu.Unlock()
	opened = true
	return s, nil
}
