// Codex app-server protocol (Apache-2.0): https://github.com/openai/codex.
// Independent Go implementation. See THIRD_PARTY_NOTICES.md.

package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type adapter struct{ executable, model, effort string }

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
	params := threadParams{Cwd: workdir, Model: a.model, DeveloperInstructions: config.SystemPrompt,
		ApprovalPolicy: "never", Sandbox: "danger-full-access", Config: overrides, ThreadID: config.ResumeToken}
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
	s.mu.Lock()
	s.token = result.Thread.ID
	s.mu.Unlock()
	opened = true
	return s, nil
}
