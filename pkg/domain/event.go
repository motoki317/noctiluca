package domain

import (
	"strings"
	"time"
)

type ConversationRef string

func NewConversationRef(name, channel, target string) ConversationRef {
	return ConversationRef(name + ":" + channel + ":" + target)
}

func ParseConversationRef(ref ConversationRef) (name, channel, target string, ok bool) {
	parts := strings.Split(string(ref), ":")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

type InboundEventKind string

const (
	InboundEventMessage       InboundEventKind = "message"
	InboundEventMessageEdited InboundEventKind = "message_edited"
	InboundEventReaction      InboundEventKind = "reaction"
)

type SenderKind string

const (
	SenderHuman SenderKind = "human"
	SenderBot   SenderKind = "bot"
)

type Sender struct {
	ID          string
	DisplayName string
	Kind        SenderKind
}

type Attachment struct {
	Ref      string
	Name     string
	MIMEType string
}

type InboundEvent struct {
	// SourceKey identifies the platform event. The inbox stores one row per key. The core fills an empty one.
	SourceKey string
	// Accepted is the core's accept decision. Adapters leave it nil.
	Accepted *bool
	Deleted  bool
	Kind     InboundEventKind
	// Conversation is where a reply goes. Parent is a thread's channel and is empty at top level.
	Conversation ConversationRef
	Parent       ConversationRef
	// MessageRef names this message, or the message that a reaction is on.
	MessageRef ConversationRef
	Direct     bool
	// AlsoInChannel marks a thread reply that its sender also posted to the channel.
	AlsoInChannel bool
	Sender        Sender
	Text          string
	Attachments   []Attachment
	MentionsMe    bool
	ReceivedAt    time.Time
}
