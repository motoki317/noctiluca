package domain

import (
	"context"
	"encoding/json"
)

type SessionKey string

const MainSessionKey SessionKey = "main"

type MCPEndpoint struct {
	URL         string
	BearerToken string
	// MainSessionTools contains the sorted names of tools to withhold from subagents.
	MainSessionTools []string
}

type ModelSessionConfig struct {
	Key          SessionKey
	ResumeToken  string
	SystemPrompt string
	ToolHub      MCPEndpoint
	Workdir      string
	// OnCompactionDone runs when a requested compaction ends, whether it succeeded or failed.
	// It is optional and must return immediately.
	OnCompactionDone func()
}

type UserInput struct {
	ID          int64
	Text        string
	Attachments []Attachment
	// SentDuringTurn reports a chat message sent while a turn was running,
	// unless its header already places it before a later reply.
	SentDuringTurn bool `json:",omitempty"`
}

type ModelEventKind string

const (
	ModelEventInputStarted   ModelEventKind = "input_started"
	ModelEventInputRefused   ModelEventKind = "input_refused"
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
	InputID  int64
	Text     string
	ToolCall *ToolCall
	Err      error
}

type ModelAdapter interface {
	Open(ctx context.Context, config ModelSessionConfig) (ModelSession, error)
}

type ModelSession interface {
	// Send can be called during a turn. The adapter adds the input to the running turn or queues it.
	// For each Send that returns nil, the session emits exactly one ModelEventInputStarted or
	// ModelEventInputRefused for the input's ID, unless it closes first. A refusal carries Err.
	// Several inputs can await acknowledgement, and acknowledgements can arrive in any order.
	// InputStarted follows TurnStarted. It can arrive after TurnFinished.
	Send(ctx context.Context, input UserInput) error
	Events() <-chan ModelEvent
	ResumeToken() string
	// ContextTokens reports the size of the session's own context after its last model request or compaction,
	// or zero if unknown. From a Compact call until no requested compaction is pending or running, it reports
	// zero. The adapter picks when compaction runs, so the caller cannot tell whether the next input reaches
	// the model before or after it.
	// Subagent contexts do not count.
	ContextTokens() int
	// Compact requests compaction between turns and returns immediately.
	Compact()
	// Idle excludes queued inputs, running turns, and pending background tasks.
	Idle() bool
	Close() error
}
