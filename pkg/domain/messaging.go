package domain

import (
	"context"
	"regexp"
	"time"
)

var messagingNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func ValidMessagingName(name string) bool { return messagingNamePattern.MatchString(name) }

type SourceInfo struct {
	Location     string
	Conversation string
	Sender       string
	ThreadAt     time.Time
	ThreadText   string
}

// SourceInfoProvider supplies readable names and an adapter-native location line.
type SourceInfoProvider interface {
	SourceInfo(ctx context.Context, event InboundEvent, fetchRoot bool) SourceInfo
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
	ToolNotes(context.Context) string
}

// UserGroupProvider refreshes accept-rule membership.
type UserGroupProvider interface {
	UserGroups(ctx context.Context, ids []string) (map[string][]string, error)
}

// DirectMessageProvider resolves owner notification destinations.
type DirectMessageProvider interface {
	DirectMessage(ctx context.Context, userID string) (ConversationRef, error)
}

// ConversationFilter avoids remote lookups in ignored conversations.
type ConversationFilter interface {
	SetConversationFilter(accepts func(ConversationRef) bool)
}

const PostingGuidance = "Post only when someone addresses you (a mention, a request to you, or a DM), or to report requested work. " +
	"If the message arrived before your last reply, answer only what it adds. Do not repeat your previous reply. " +
	"If a follow-up asks to append a phrase, send only that phrase once the answer is posted."
