package domain

import (
	"context"
	"encoding/json"
)

type SessionKey string

type MCPEndpoint struct {
	URL         string
	BearerToken string
}

type ModelSessionConfig struct {
	Key          SessionKey
	ResumeToken  string
	SystemPrompt string
	ToolHub      MCPEndpoint
	Workdir      string
}

type UserInput struct {
	Text        string
	Attachments []Attachment
}

type ModelEventKind string

const (
	ModelEventTurnStarted    ModelEventKind = "turn_started"
	ModelEventText           ModelEventKind = "text"
	ModelEventToolCall       ModelEventKind = "tool_call"
	ModelEventToolResult     ModelEventKind = "tool_result"
	ModelEventBackgroundDone ModelEventKind = "background_done"
	ModelEventTurnFinished   ModelEventKind = "turn_finished"
	ModelEventError          ModelEventKind = "error"
)

type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

type ModelEvent struct {
	Kind     ModelEventKind
	Text     string
	ToolCall *ToolCall
	Err      error
}

type ModelAdapter interface {
	Open(ctx context.Context, config ModelSessionConfig) (ModelSession, error)
}

type ModelSession interface {
	Send(ctx context.Context, input UserInput) error
	Events() <-chan ModelEvent
	ResumeToken() string
	// Idle excludes queued inputs, running turns, and pending background tasks.
	Idle() bool
	Close() error
}
