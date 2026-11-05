package domain

import "time"

type ConversationRef string

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
	Sender       Sender
	Text         string
	Attachments  []Attachment
	MentionsMe   bool
	ReceivedAt   time.Time
}
