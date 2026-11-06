package domain

import (
	"context"
	"time"
)

type SourceInfo struct {
	Conversation string
	Sender       string
	ThreadAt     time.Time
	ThreadText   string
}

// SourceInfoProvider resolves opaque refs for readable input headers.
type SourceInfoProvider interface {
	SourceInfo(context.Context, ConversationRef, string, bool) SourceInfo
}

type OutboundMessage struct {
	Text        string
	Attachments []Attachment
}

type MessagingAdapter interface {
	Shutdown(context.Context) error
	Start(ctx context.Context, consume func(context.Context, InboundEvent) error) error
	Send(ctx context.Context, conversation ConversationRef, message OutboundMessage) (string, error)
	Tools() []Tool
	ToolNotes() string
}

// UserGroupProvider refreshes accept-rule membership.
type UserGroupProvider interface {
	UserGroups(ctx context.Context, ids []string) (map[string][]string, error)
}

// DirectMessageProvider resolves owner notifications and task replies.
type DirectMessageProvider interface {
	DirectMessage(context.Context, string) (ConversationRef, error)
}

// ConversationFilter avoids sender lookups for reactions in unaccepted channels.
type ConversationFilter interface {
	SetConversationFilter(func(ConversationRef, bool) bool)
}
