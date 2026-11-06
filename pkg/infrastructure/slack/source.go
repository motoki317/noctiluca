package slack

import (
	"context"
	"strconv"
	"strings"
	"time"

	sdk "github.com/slack-go/slack"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func timestamp(value string) time.Time {
	seconds, fraction, _ := strings.Cut(value, ".")
	sec, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return time.Time{}
	}
	nanos, err := strconv.ParseInt((fraction + "000000000")[:9], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, nanos)
}

func (a *adapter) userName(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	if name, ok := a.names.Load(id); ok {
		return name.(string)
	}
	name := id
	if user, err := a.bot.GetUserInfoContext(ctx, id); err == nil {
		for _, candidate := range []string{user.Profile.DisplayName, user.RealName, user.Name} {
			if candidate != "" {
				name = candidate
				break
			}
		}
	}
	if name != id {
		a.names.Store(id, name)
	}
	return name
}

func (a *adapter) SourceInfo(ctx context.Context, ref domain.ConversationRef, sender string, root bool) domain.SourceInfo {
	channel, thread, err := destination(ref)
	if err != nil {
		return domain.SourceInfo{Conversation: string(ref), Sender: sender}
	}
	info := domain.SourceInfo{Conversation: channel, Sender: a.userName(ctx, sender), ThreadAt: timestamp(thread)}
	if name, ok := a.names.Load(channel); ok {
		info.Conversation = name.(string)
	} else {
		params := &sdk.GetConversationInfoInput{ChannelID: channel}
		conversation, err := a.bot.GetConversationInfoContext(ctx, params)
		if a.fallbackReader(err) {
			conversation, err = a.reader.GetConversationInfoContext(ctx, params)
		}
		if err == nil {
			if conversation.IsIM {
				info.Conversation = a.userName(ctx, conversation.User)
			} else if conversation.Name != "" {
				info.Conversation = "#" + conversation.Name
			}
		} else if strings.HasPrefix(channel, "D") && info.Sender != "" {
			info.Conversation = info.Sender
		}
		if err == nil && info.Conversation != channel && (!conversation.IsIM || info.Conversation != conversation.User) {
			a.names.Store(channel, info.Conversation)
		}
	}
	if root && thread != "" {
		params := &sdk.GetConversationRepliesParameters{ChannelID: channel, Timestamp: thread, Limit: 1}
		messages, _, _, err := a.bot.GetConversationRepliesContext(ctx, params)
		if a.fallbackReader(err) {
			messages, _, _, err = a.reader.GetConversationRepliesContext(ctx, params)
		}
		if err == nil && len(messages) > 0 && messages[0].Timestamp == thread {
			info.ThreadText = messages[0].Text
		}
	}
	return info
}
