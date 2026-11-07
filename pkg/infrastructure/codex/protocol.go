// Codex app-server protocol (Apache-2.0): https://github.com/openai/codex.
// Independent Go implementation. See THIRD_PARTY_NOTICES.md.

package codex

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type rpcFrame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("codex RPC error %d: %s", e.Code, e.Message) }

type threadParams struct {
	ThreadID              string         `json:"threadId,omitempty"`
	ExcludeTurns          bool           `json:"excludeTurns,omitempty"`
	Cwd                   string         `json:"cwd"`
	Model                 string         `json:"model,omitempty"`
	DeveloperInstructions string         `json:"developerInstructions"`
	ApprovalPolicy        string         `json:"approvalPolicy"`
	Sandbox               string         `json:"sandbox"`
	Config                map[string]any `json:"config"`
}

type threadResult struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
}

type turn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type notification struct {
	WillRetry bool            `json:"willRetry"`
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId"`
	Turn      turn            `json:"turn"`
	Delta     string          `json:"delta"`
	Item      json.RawMessage `json:"item"`
	Error     struct {
		Message string `json:"message"`
	} `json:"error"`
}

func notificationEvents(method string, n notification) ([]domain.ModelEvent, error) {
	switch method {
	case "turn/started":
		return []domain.ModelEvent{{Kind: domain.ModelEventTurnStarted}}, nil
	case "turn/completed":
		var events []domain.ModelEvent
		if n.Turn.Error != nil {
			events = append(events, domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New(n.Turn.Error.Message)})
		} else if n.Turn.Status == "failed" || n.Turn.Status == "interrupted" {
			events = append(events, domain.ModelEvent{Kind: domain.ModelEventError, Err: fmt.Errorf("codex turn %s", n.Turn.Status)})
		}
		return append(events, domain.ModelEvent{Kind: domain.ModelEventTurnFinished}), nil
	case "item/agentMessage/delta":
		return []domain.ModelEvent{{Kind: domain.ModelEventText, Text: n.Delta}}, nil
	case "error":
		if n.WillRetry {
			return nil, nil
		}
		return []domain.ModelEvent{{Kind: domain.ModelEventError, Err: errors.New(n.Error.Message)}}, nil
	case "item/started", "item/completed":
		var item struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Server    string          `json:"server"`
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
			Status    string          `json:"status"`
		}
		if err := json.Unmarshal(n.Item, &item); err != nil {
			return nil, errors.New("invalid codex item")
		}
		call := &domain.ToolCall{ID: item.ID, Name: item.Type, Input: n.Item}
		switch item.Type {
		case "mcpToolCall":
			call.Name, call.Input = "mcp__"+item.Server+"__"+item.Tool, item.Arguments
		case "commandExecution", "fileChange", "webSearch", "dynamicToolCall":
		default:
			return nil, nil
		}
		event := domain.ModelEvent{Kind: domain.ModelEventToolCall, ToolCall: call}
		if method == "item/completed" {
			event.Kind = domain.ModelEventToolResult
			event.ToolCall = &domain.ToolCall{ID: item.ID}
			if item.Status == "failed" || item.Status == "declined" {
				event.Err = errors.New("codex tool " + item.Status)
			}
		}
		return []domain.ModelEvent{event}, nil
	}
	return nil, nil
}
