package domain

import (
	"context"
	"regexp"
	"time"
)

var messagingNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func ValidMessagingName(name string) bool { return messagingNamePattern.MatchString(name) }

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

type NamedRef struct {
	Name string
	Ref  ConversationRef
}

type PostRefProvider interface {
	// PostRefs names the refs besides the conversation that an input can be answered at.
	PostRefs(conversation, message ConversationRef, direct bool) []NamedRef
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

// ConversationFilter avoids remote lookups in ignored conversations.
type ConversationFilter interface {
	SetConversationFilter(func(ConversationRef) bool)
}
