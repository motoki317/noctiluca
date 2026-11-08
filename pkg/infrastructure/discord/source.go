package discord

import (
	"context"
	"errors"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (a *adapter) SourceInfo(ctx context.Context, ref domain.ConversationRef, senderID string, root bool) domain.SourceInfo {
	info := domain.SourceInfo{Conversation: string(ref), Sender: senderID}
	channel, thread, err := a.destination(ref)
	if err != nil {
		return info
	}
	info.Conversation = channel.String()
	userID, userErr := parseID(senderID)
	if userErr == nil {
		if user, err := a.api.GetUser(userID, rest.WithCtx(ctx)); err == nil {
			info.Sender = user.EffectiveName()
		}
	}
	if c, err := a.api.GetChannel(channel, rest.WithCtx(ctx)); err == nil {
		if c.Name() != "" {
			info.Conversation = "#" + c.Name()
		}
		if c.Type() == sdk.ChannelTypeDM {
			info.Conversation = info.Sender
		}
		if guild, ok := c.(sdk.GuildChannel); ok && userErr == nil {
			if member, err := a.api.GetMember(guild.GuildID(), userID, rest.WithCtx(ctx)); err == nil {
				info.Sender = member.EffectiveName()
			}
		}
	}
	if thread != 0 {
		info.ThreadAt = thread.Time()
		if root {
			if m, err := a.api.GetMessage(channel, thread, rest.WithCtx(ctx)); err == nil {
				info.ThreadText = m.Content
				info.ThreadAt = m.CreatedAt
			}
		}
	}
	return info
}

func (a *adapter) UserGroups(ctx context.Context, ids []string) (map[string][]string, error) {
	groups := make(map[string][]string, len(ids))
	for _, id := range ids {
		if _, err := parseID(id); err != nil {
			return nil, err
		}
		groups[id] = []string{}
	}
	if len(ids) == 0 {
		return groups, nil
	}
	if !a.config.NeedGroups {
		return nil, errors.New("roles require the Server Members intent for Discord")
	}
	var after snowflake.ID
	for {
		var guilds []sdk.OAuth2Guild
		previous := after
		query := sdk.QueryValues{"limit": 200, "after": after}
		// Disgo's OAuth2 helper replaces bot authentication with a bearer token.
		if err := a.client.Do(rest.GetCurrentUserGuilds.Compile(query), nil, &guilds, rest.WithCtx(ctx)); err != nil {
			return nil, discordError("list Discord guilds failed", err)
		}
		for _, guild := range guilds {
			if err := a.roleMembers(ctx, guild.ID, groups); err != nil {
				return nil, err
			}
			if guild.ID > after {
				after = guild.ID
			}
		}
		if len(guilds) < 200 {
			break
		}
		if after <= previous {
			return nil, errors.New("guild cursor did not advance for Discord")
		}
	}
	return groups, nil
}

func (a *adapter) roleMembers(ctx context.Context, guild snowflake.ID, groups map[string][]string) error {
	var after snowflake.ID
	for {
		members, err := a.api.GetMembers(guild, 1000, after, rest.WithCtx(ctx))
		if err != nil {
			return discordError("list Discord role members failed", err)
		}
		previous := after
		for _, member := range members {
			if _, ok := groups[guild.String()]; ok {
				groups[guild.String()] = append(groups[guild.String()], member.User.ID.String())
			}
			for _, role := range member.RoleIDs {
				if _, ok := groups[role.String()]; ok {
					groups[role.String()] = append(groups[role.String()], member.User.ID.String())
				}
			}
			if member.User.ID > after {
				after = member.User.ID
			}
		}
		if len(members) < 1000 {
			return nil
		}
		if after <= previous {
			return errors.New("member cursor did not advance for Discord")
		}
	}
}
