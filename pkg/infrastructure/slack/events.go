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
	var channel, thread, reactionUser string
	var mentionsMe bool
	switch e := data.(type) {
	case *slackevents.MessageEvent:
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
		channel, thread = e.Channel, m.ThreadTimestamp
		if a.isSelf(m.User, m.BotID) {
			if thread == "" {
				thread = m.Timestamp
			}
			if channel != "" && thread != "" {
				a.joinedThreads.Store(Conversation(a.name, channel, thread), struct{}{})
			}
			return event, false
		}
		// Unfurls change attachments without changing the user's message.
		if e.SubType == "message_changed" && e.Message != nil && e.PreviousMessage != nil && e.Message.Text == e.PreviousMessage.Text && slices.EqualFunc(e.Message.Files, e.PreviousMessage.Files, func(a, b sdk.File) bool { return a.ID == b.ID }) {
			return event, false
		}
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
		mentionsMe = a.selfUser != "" && strings.Contains(m.Text, "<@"+a.selfUser+">")
		event = domain.InboundEvent{MessageRef: Conversation(a.name, channel, m.Timestamp), Kind: kind, Deleted: deleted, Sender: sender, Text: m.Text, MentionsMe: mentionsMe, ReceivedAt: time.Now()}
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
			event.Text = ""
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
		reactionUser = e.ItemUser
		sender := domain.Sender{ID: e.User, Kind: domain.SenderBot}
		// The target timestamp keeps the reaction useful even when its message is unavailable.
		event = domain.InboundEvent{MessageRef: Conversation(a.name, channel, e.Item.Timestamp), Kind: domain.InboundEventReaction, Sender: sender, Text: fmt.Sprintf(":%s: on message %s", e.Reaction, e.Item.Timestamp), ReceivedAt: time.Now()}
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
	event.Conversation = Conversation(a.name, channel, thread)
	if thread != "" {
		event.Parent = Conversation(a.name, channel, "")
	}
	acceptsConversation := a.acceptConversation == nil || a.acceptConversation(Conversation(a.name, channel, ""))
	if !event.Direct {
		switch {
		case event.Kind == domain.InboundEventReaction:
			if reactionUser != a.selfUser {
				return event, false
			}
		case thread != "" && !mentionsMe && acceptsConversation:
			joined, err := a.joinedThread(ctx, channel, thread)
			if err != nil {
				slog.WarnContext(ctx, "Thread participation lookup failed", "channel", channel, "thread", thread, "error", err)
			} else if !joined {
				return event, false
			}
		}
	}
	if event.Kind == domain.InboundEventReaction && acceptsConversation {
		// Unknown senders must not bypass the core's bot restriction.
		if user, err := a.bot.GetUserInfoContext(ctx, event.Sender.ID); err == nil {
			if !user.IsBot {
				event.Sender.Kind = domain.SenderHuman
			}
			event.Sender.DisplayName = user.Profile.DisplayName
		} else {
			slog.WarnContext(ctx, "Reaction sender lookup failed", "channel", channel, "error", err)
		}
	}
	return event, true
}

func (a *adapter) isSelf(user, bot string) bool {
	return (a.selfUser != "" && user == a.selfUser) || (a.selfBot != "" && bot == a.selfBot)
}

func (a *adapter) joinedThread(ctx context.Context, channel, thread string) (bool, error) {
	ref := Conversation(a.name, channel, thread)
	if _, ok := a.joinedThreads.Load(ref); ok {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	params := &sdk.GetConversationRepliesParameters{ChannelID: channel, Timestamp: thread, Limit: 100}
	reader := a.bot
	for {
		messages, _, cursor, err := reader.GetConversationRepliesContext(ctx, params)
		if reader == a.bot && a.fallbackReader(err) {
			reader = a.reader
			messages, _, cursor, err = reader.GetConversationRepliesContext(ctx, params)
		}
		if err != nil {
			return false, err
		}
		for _, m := range messages {
			if a.isSelf(m.User, m.BotID) {
				a.joinedThreads.Store(ref, struct{}{})
				return true, nil
			}
		}
		if cursor == "" {
			return false, nil
		}
		params.Cursor = cursor
	}
}
