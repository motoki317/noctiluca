package domain

import (
	"context"
	"time"
)

type InboxPayload struct {
	Target        ConversationRef `json:",omitempty"`
	Continuations int             `json:",omitempty"`
	Procedure     string          `json:",omitempty"`
	Notice        string          `json:",omitempty"`
	Input         UserInput
	Context       SessionContext
	Event         *InboundEvent `json:",omitempty"`
}

type InboxItem struct {
	ID         int64
	SourceKey  string
	SessionKey SessionKey
	Payload    InboxPayload
	ReadyAt    time.Time
	Lane       string
	Status     string
	Attempts   int
	RetryAt    time.Time
	Deadline   time.Time
	LastError  string
}

func NewOwnerNotice(source, text string) InboxItem {
	return InboxItem{
		SourceKey:  source,
		SessionKey: SessionKey("notice:" + source),
		Lane:       "conv",
		ReadyAt:    time.Now(),
		Payload: InboxPayload{
			Notice: text,
			// Owner notices must pass acceptance without a conversation rule.
			Context: SessionContext{TaskName: "owner-notice"},
		},
	}
}

// InboxRepository commits source outcomes and their next input together.
type InboxRepository interface {
	Delivered(context.Context, int64, Session) error
	Reject(context.Context, int64, string) error
	Enqueue(context.Context, InboxItem) (int64, error)
	Receive(context.Context, InboundEvent, InboxItem) error
	Active(context.Context) ([]InboxItem, error)
	Claim(context.Context, int64, time.Time) (bool, error)
	Finish(context.Context, int64, string, int, time.Time) (bool, error)
	Fire(context.Context, Schedule, time.Time, *InboxItem) error
}

type Watch struct {
	ID      int64
	Command string
	CWD     string
	Item    InboxItem
}

type WatchRepository interface {
	Add(context.Context, Watch) (int64, error)
	Unfinished(context.Context) ([]Watch, error)
	Complete(context.Context, int64, string, time.Time) error
}
