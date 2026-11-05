package toolhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/motoki317/noctiluca/pkg/domain"
)

type preparedTool struct {
	tool   domain.Tool
	schema *jsonschema.Resolved
}

func (h *hub) AddSource(name string, tools []domain.Tool) error {
	return h.setSource(name, tools, false)
}

func (h *hub) ReplaceSource(name string, tools []domain.Tool) error {
	return h.setSource(name, tools, true)
}

func (h *hub) setSource(name string, tools []domain.Tool, replace bool) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("tool source name is required")
	}
	prepared := make([]preparedTool, 0, len(tools))
	names := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if !domain.ValidToolName(tool.Name) {
			return errors.New("invalid tool name")
		}
		if tool.Handler == nil {
			return fmt.Errorf("tool %s has no handler", tool.Name)
		}
		if _, ok := names[tool.Name]; ok {
			return fmt.Errorf("duplicate tool %s", tool.Name)
		}
		names[tool.Name] = struct{}{}
		var schema jsonschema.Schema
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil || schema.Type != "object" {
			return fmt.Errorf("tool %s requires an object input schema", tool.Name)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return fmt.Errorf("invalid schema for tool %s: %w", tool.Name, err)
		}
		tool.InputSchema = append(json.RawMessage(nil), tool.InputSchema...)
		prepared = append(prepared, preparedTool{tool: tool, schema: resolved})
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("tool hub is closed")
	}
	if _, ok := h.sources[name]; ok && !replace {
		return errors.New("tool source already registered")
	}
	for tool := range names {
		if source, ok := h.tools[tool]; ok && source != name {
			return fmt.Errorf("tool %s already registered", tool)
		}
	}
	// Validate the whole source before adding anything. A collision must not replace permissions.
	for tool, source := range h.tools {
		if _, exists := names[tool]; source == name && !exists {
			h.server.RemoveTools(tool)
			delete(h.tools, tool)
		}
	}
	for _, p := range prepared {
		h.server.AddTool(&mcp.Tool{Name: p.tool.Name, Description: p.tool.Description, InputSchema: p.tool.InputSchema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: p.tool.ReadOnly}}, p.call)
		h.tools[p.tool.Name] = name
	}
	h.sources[name] = struct{}{}
	return nil
}

func toolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: message}}}
}

func (p preparedTool) call(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	g, ok := ctx.Value(grantKey{}).(*grant)
	if !ok || g.ctx.Err() != nil {
		return toolError("Session is closed"), nil
	}
	if p.tool.OwnerOnly && g.session.Actor != domain.ActorOwner {
		return toolError("Owner permission required"), nil
	}
	input := req.Params.Arguments
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		return toolError("Invalid tool arguments"), nil
	}
	if err := p.schema.Validate(value); err != nil {
		return toolError("Invalid tool arguments"), nil
	}
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.ctx, cancel)
	defer stop()
	defer cancel()
	if g.ctx.Err() != nil {
		return toolError("Session is closed"), nil
	}
	output, err := p.tool.Handler(callCtx, input)
	if err != nil {
		return toolError(err.Error()), nil
	}
	if !json.Valid(output) {
		return toolError("Tool returned invalid JSON"), nil
	}
	text := string(output)
	if p.tool.TextResult {
		if !strings.HasPrefix(strings.TrimSpace(text), `"`) || json.Unmarshal(output, &text) != nil {
			return toolError("Tool returned invalid text"), nil
		}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
}
