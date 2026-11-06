package slack

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	sdk "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (a *adapter) inbound(ctx context.Context, data any) (domain.InboundEvent, bool) {
	var event domain.InboundEvent
	var channel, thread string
	switch e := data.(type) {
	case *slackevents.MessageEvent:
		// Unfurls change attachments without changing the user's message.
		if e.SubType == "message_changed" && e.Message != nil && e.PreviousMessage != nil && e.Message.Text == e.PreviousMessage.Text && slices.EqualFunc(e.Message.Files, e.PreviousMessage.Files, func(a, b sdk.File) bool { return a.ID == b.ID }) {
			return event, false
		}
		kind := domain.InboundEventMessage
		switch e.SubType {
		case "", "bot_message", "file_share", "me_message", "thread_broadcast":
		case "message_changed", "message_deleted":
			kind = domain.InboundEventMessageEdited
		default:
			return event, false
		}
		m := e.Message
		deleted := e.SubType == "message_deleted"
		if deleted {
			m = e.PreviousMessage
			if m == nil {
				return event, false
			}
		}
		if m == nil {
			m = &sdk.Msg{User: e.User, BotID: e.BotID, Text: e.Text, ThreadTimestamp: e.ThreadTimeStamp, Timestamp: e.TimeStamp, Username: e.Username}
		}
		if (a.selfUser != "" && m.User == a.selfUser) || (a.selfBot != "" && m.BotID == a.selfBot) {
			return event, false
		}
		channel, thread = e.Channel, m.ThreadTimestamp
		if thread == m.Timestamp {
			thread = ""
		}
		sender := domain.Sender{ID: m.User, DisplayName: m.Username, Kind: domain.SenderHuman}
		if m.BotID != "" {
			sender.Kind = domain.SenderBot
			if sender.ID == "" {
				sender.ID = m.BotID
			}
		}
		event = domain.InboundEvent{MessageRef: Conversation(channel, m.Timestamp), Kind: kind, Deleted: deleted, Sender: sender, Text: m.Text, MentionsMe: a.selfUser != "" && strings.Contains(m.Text, "<@"+a.selfUser+">"), ReceivedAt: time.Now()}
		posted := m.Timestamp
		if kind == domain.InboundEventMessageEdited {
			posted = e.EventTimeStamp
			if m.Edited != nil && !deleted {
				posted = m.Edited.Timestamp
			}
		}
		if at := timestamp(posted); !at.IsZero() {
			event.ReceivedAt = at
		}
		if deleted {
			event.Text = "[message deleted]"
			event.MentionsMe = false
		}
		for _, f := range m.Files {
			if deleted {
				break
			}
			event.Attachments = append(event.Attachments, domain.Attachment{Ref: f.ID, Name: f.Name, MIMEType: f.Mimetype})
		}
	case *slackevents.ReactionAddedEvent:
		if e.User == a.selfUser || e.Item.Type != "message" {
			return event, false
		}
		channel = e.Item.Channel
		sender := domain.Sender{ID: e.User, Kind: domain.SenderBot}
		if a.acceptConversation == nil || a.acceptConversation(Conversation(channel, ""), strings.HasPrefix(channel, "D")) {
			// Unknown senders must not bypass the core's bot restriction.
			if user, err := a.bot.GetUserInfoContext(ctx, e.User); err == nil {
				if !user.IsBot {
					sender.Kind = domain.SenderHuman
				}
				sender.DisplayName = user.Profile.DisplayName
			} else {
				slog.WarnContext(ctx, "Reaction sender lookup failed", "channel", channel, "error", err)
			}

		}
		// The target timestamp keeps the reaction useful even when its message is unavailable.
		event = domain.InboundEvent{MessageRef: Conversation(channel, e.Item.Timestamp), Kind: domain.InboundEventReaction, Sender: sender, Text: fmt.Sprintf(":%s: on message %s", e.Reaction, e.Item.Timestamp), ReceivedAt: time.Now()}
		if at := timestamp(e.EventTimestamp); !at.IsZero() {
			event.ReceivedAt = at
		}
	default:
		return event, false
	}
	if channel == "" || event.Sender.ID == "" {
		return event, false
	}
	event.Direct = strings.HasPrefix(channel, "D")
	event.Conversation = Conversation(channel, thread)
	if thread != "" {
		event.Parent = Conversation(channel, "")
	}
	return event, true
}
