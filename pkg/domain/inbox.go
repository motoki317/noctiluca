package domain

import (
	"context"
	"strings"
	"time"
)

const NoticeSessionPrefix = "notice:"

func (key SessionKey) IsNotice() bool {
	return strings.HasPrefix(string(key), NoticeSessionPrefix)
}

type InboxPayload struct {
	// Continuations counts the interruption notes that led to this input.
	Continuations int    `json:",omitempty"`
	Procedure     string `json:",omitempty"`
	Notice        string `json:",omitempty"`
	Input         UserInput
	Context       SessionContext
	Event         *InboundEvent `json:",omitempty"`
}

type InboxStatus string

const (
	// The row waits for dispatch or for its retry time.
	InboxPending InboxStatus = "pending"
	// The reconciler claimed the row for a model process, and the model has not started on it.
	InboxDispatched InboxStatus = "dispatched"
	// Nothing is left to send: the model started on the input, the notice went out, or the core did not accept the event.
	// noctiluca does not send a started input again, because chat posts are not idempotent.
	// Only a crash before this status is stored can repeat one input.
	InboxDone InboxStatus = "done"
	// The row failed for good.
	InboxDead InboxStatus = "dead"
)

type Lane string

const (
	LaneConversation Lane = "conv"
	// A task row gets a task header instead of a chat header. The lane does not order dispatch.
	LaneTask Lane = "task"
)

type InboxItem struct {
	ID         int64
	SourceKey  string
	SessionKey SessionKey
	Payload    InboxPayload
	ReadyAt    time.Time
	Lane       Lane
	Status     InboxStatus
	Attempts   int
	RetryAt    time.Time
	LastError  string
}

func NewOwnerNotice(source, text string) InboxItem {
	return InboxItem{
		SourceKey:  source,
		SessionKey: SessionKey(NoticeSessionPrefix + source),
		Lane:       LaneConversation,
		ReadyAt:    time.Now(),
		Payload: InboxPayload{
			Notice: text,
			// The empty Conversation lets a notice pass the accept check.
			Context: SessionContext{TaskName: "owner-notice"},
		},
	}
}

// Each method commits all of its writes in one transaction: an event with its row,
// a schedule's next run with its row, a delivery with its session state.
type InboxRepository interface {
	Delivered(ctx context.Context, id int64, session Session) error
	Reject(ctx context.Context, id int64, reason string) error
	Enqueue(ctx context.Context, item InboxItem) (int64, error)
	Receive(ctx context.Context, event InboundEvent, item InboxItem) error
	Active(ctx context.Context) ([]InboxItem, error)
	Claim(ctx context.Context, id int64) (bool, error)
	Fail(ctx context.Context, id int64, failure string, maxAttempts int, retryAt time.Time) (bool, error)
	Fire(ctx context.Context, schedule Schedule, next time.Time, item *InboxItem) error
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
