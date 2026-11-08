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
	SourceKey    string
	Accepted     *bool
	Deleted      bool
	Kind         InboundEventKind
	Conversation ConversationRef
	Parent       ConversationRef
	MessageRef   ConversationRef
	Direct       bool
	Sender       Sender
	Text         string
	Attachments  []Attachment
	MentionsMe   bool
	ReceivedAt   time.Time
}
