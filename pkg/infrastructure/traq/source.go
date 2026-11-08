package traq

import (
	"context"
	"slices"
	"strings"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func sender(id, name string, bot bool) domain.Sender {
	kind := domain.SenderHuman
	if bot {
		kind = domain.SenderBot
	}
	if name == "" {
		name = id
	}
	return domain.Sender{ID: id, DisplayName: name, Kind: kind}
}

func (a *adapter) user(ctx context.Context, id string) domain.Sender {
	user, err := a.lookupUser(ctx, id)
	if err != nil {
		// Source headers can fall back to IDs without changing event acceptance.
		return sender(id, id, true)
	}
	return user
}

func (a *adapter) lookupUser(ctx context.Context, id string) (domain.Sender, error) {
	if cached, ok := a.users.Load(id); ok {
		return cached.(domain.Sender), nil
	}
	result, response, err := a.api.UserAPI.GetUser(ctx, id).Execute()
	if err != nil {
		return domain.Sender{}, apiError("read traQ user", response, err)
	}
	name := result.DisplayName
	if name == "" {
		name = result.Name
	}
	user := sender(id, name, result.Bot)
	a.users.Store(id, user)
	return user, nil
}

func (a *adapter) channelName(ctx context.Context, id string) string {
	if user, ok := a.direct.Load(id); ok && user.(string) != "" {
		return a.user(ctx, user.(string)).DisplayName
	}
	if cached, ok := a.channels.Load(id); ok {
		return cached.(string)
	}
	parts := []string{}
	seen := make(map[string]bool)
	for current := id; current != ""; {
		if seen[current] {
			return id
		}
		seen[current] = true
		channel, _, err := a.api.ChannelAPI.GetChannel(ctx, current).Execute()
		if err != nil || channel.Name == "" {
			return id
		}
		parts = append(parts, channel.Name)
		current = channel.GetParentId()
	}
	slices.Reverse(parts)
	name := "#" + strings.Join(parts, "/")
	a.channels.Store(id, name)
	return name
}

func (a *adapter) SourceInfo(ctx context.Context, ref domain.ConversationRef, user string, root bool) domain.SourceInfo {
	info := domain.SourceInfo{Conversation: string(ref), Sender: user}
	channel, message, err := a.destination(ref)
	if err != nil {
		return info
	}
	info.Conversation = a.channelName(ctx, channel)
	if user != "" {
		info.Sender = a.user(ctx, user).DisplayName
	}
	if root && message != "" {
		if result, _, err := a.api.MessageAPI.GetMessage(ctx, message).Execute(); err == nil && result.ChannelId == channel {
			info.ThreadAt, info.ThreadText = result.CreatedAt, result.Content
		}
	}
	return info
}
