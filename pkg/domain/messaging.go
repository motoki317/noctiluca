package domain

import "context"

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

type UserGroupProvider interface {
	UserGroups(ctx context.Context, ids []string) (map[string][]string, error)
}

type DirectConversationProvider interface {
	DirectConversation(ConversationRef) bool
}

type DirectMessageProvider interface {
	DirectMessage(context.Context, string) (ConversationRef, error)
}

// ConversationFilter avoids remote lookups for channels without an accept rule.
type ConversationFilter interface {
	SetConversationFilter(func(ConversationRef) bool)
}
