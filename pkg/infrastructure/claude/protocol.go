// Claude CLI protocol reference: OpenClaw (MIT), https://github.com/openclaw/openclaw.
// Go reimplementation. Provenance and license notice: THIRD_PARTY_NOTICES.md.

package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/motoki317/noctiluca/pkg/domain"
)

const authRemedy = "check `claude auth status`; run `claude /login` if the login expired"

type frame struct {
	Result           string   `json:"result"`
	Errors           []string `json:"errors"`
	Type             string   `json:"type"`
	Subtype          string   `json:"subtype"`
	SessionID        string   `json:"session_id"`
	ParentToolUseID  string   `json:"parent_tool_use_id"`
	State            string   `json:"state"`
	CommandUUID      string   `json:"command_uuid"`
	UserMessageUUID  string   `json:"user_message_uuid"`
	UserMessageUUIDs []string `json:"user_message_uuids"`
	Status           string   `json:"status"`
	Summary          string   `json:"summary"`
	TaskID           string   `json:"task_id"`
	IsBackgrounded   bool     `json:"is_backgrounded"`
	Tasks            []struct {
		TaskID string `json:"task_id"`
	} `json:"tasks"`
	IsError   bool   `json:"is_error"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype string          `json:"subtype"`
		Input   json.RawMessage `json:"input"`
	} `json:"request"`
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
	} `json:"response"`
	Event struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Content []contentBlock `json:"-"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	IsError   bool            `json:"is_error"`
	ToolUseID string          `json:"tool_use_id"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

func (f frame) failedResult() bool {
	return f.Type == "result" && (f.IsError || strings.HasPrefix(f.Subtype, "error"))
}

func parseFrame(data []byte) (frame, error) {
	var f frame
	if err := json.Unmarshal(data, &f); err != nil || f.Type == "" {
		return f, errors.New("invalid claude stream frame")
	}
	// Replayed user text is a string; tool results arrive as user content blocks.
	if f.Type == "assistant" || (f.Type == "user" && len(f.Message.Content) > 0 && f.Message.Content[0] == '[') {
		if err := json.Unmarshal(f.Message.Content, &f.Content); err != nil {
			return f, errors.New("invalid claude message content")
		}
	}
	return f, nil
}

type parser struct {
	active  bool
	command string
	tasks   map[string]struct{}
}

func (p *parser) events(f frame) []domain.ModelEvent {
	var events []domain.ModelEvent
	start := func() {
		if !p.active {
			p.active = true
			events = append(events, domain.ModelEvent{Kind: domain.ModelEventTurnStarted})
		}
	}
	switch f.Type {
	case "command_lifecycle":
		if f.State == "started" {
			p.command = f.CommandUUID
			start()
		}
		if terminalCommand(f.State) && f.CommandUUID == p.command {
			p.active = false
		}
		if f.State == "refused" || f.State == "discarded" || f.State == "cancelled" {
			events = append(events, domain.ModelEvent{Kind: domain.ModelEventError, Err: fmt.Errorf("claude input %s", f.State)})
		}
	case "stream_event":
		if f.ParentToolUseID != "" {
			break
		}
		if f.Event.Type == "message_start" {
			start()
		}
		if f.Event.Type == "content_block_delta" && f.Event.Delta.Type == "text_delta" {
			events = append(events, domain.ModelEvent{Kind: domain.ModelEventText, Text: f.Event.Delta.Text})
		}
	case "user":
		for _, c := range f.Content {
			if c.Type == "tool_result" && c.IsError {
				events = append(events, domain.ModelEvent{Kind: domain.ModelEventToolResult, ToolCall: &domain.ToolCall{ID: c.ToolUseID}, Err: errors.New("model tool failed")})
			}
		}
	case "assistant":
		if f.ParentToolUseID != "" {
			break
		}
		start()
		// Complete tool inputs arrive here. Partial deltas and replayed text would duplicate them.
		for _, c := range f.Content {
			if c.Type == "tool_use" {
				events = append(events, domain.ModelEvent{Kind: domain.ModelEventToolCall, ToolCall: &domain.ToolCall{ID: c.ID, Name: c.Name, Input: c.Input}})
			}
		}
	case "system":
		switch f.Subtype {
		case "background_tasks_changed":
			p.tasks = make(map[string]struct{}, len(f.Tasks))
			for _, task := range f.Tasks {
				p.tasks[task.TaskID] = struct{}{}
			}
		case "task_started":
			if !f.IsBackgrounded {
				break
			}
			if p.tasks == nil {
				p.tasks = make(map[string]struct{})
			}
			p.tasks[f.TaskID] = struct{}{}
		case "task_notification":
			delete(p.tasks, f.TaskID)
		}
		if f.Subtype == "task_notification" {
			events = append(events, domain.ModelEvent{Kind: domain.ModelEventBackgroundDone, Text: fmt.Sprintf("%s (%s): %s", f.TaskID, f.Status, f.Summary)})
		}
	case "result":
		if f.failedResult() {
			// CLI error bodies can contain prompts, credentials, or tool output.
			events = append(events, domain.ModelEvent{Kind: domain.ModelEventError, Err: errors.New("claude turn failed: " + authRemedy)})
			f.Result = ""
		}
		events = append(events, domain.ModelEvent{Kind: domain.ModelEventTurnFinished, Text: f.Result})
		p.active = false
	}
	return events
}

func terminalCommand(state string) bool {
	return state == "completed" || state == "refused" || state == "discarded" || state == "cancelled"
}
