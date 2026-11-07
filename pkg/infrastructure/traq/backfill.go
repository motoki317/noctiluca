package traq

import (
	"cmp"
	"context"
	"slices"
	"time"

	sdk "github.com/traPtitech/go-traq"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type backfillTarget struct {
	channel string
	direct  bool
	since   time.Time
	until   time.Time
}

func (a *adapter) backfillTargets(ctx context.Context) []backfillTarget {
	channels := append([]string(nil), a.config.Channels...)
	ownerChannel := ""
	if a.config.Owner != "" {
		ref, err := a.DirectMessage(ctx, a.config.Owner)
		if err != nil {
			a.warnLookup(ctx, "resolve traQ direct message", err, "user_id", a.config.Owner)
		} else {
			ownerChannel, _, _ = a.destination(ref)
			channels = append(channels, ownerChannel)
		}
	}
	now := time.Now()
	targets := make([]backfillTarget, 0, len(channels))
	seen := make(map[string]bool)
	for _, channel := range channels {
		if ctx.Err() != nil {
			break
		}
		if seen[channel] {
			continue
		}
		seen[channel] = true
		if channel != ownerChannel {
			a.direct.Store(channel, "")
		}
		last, err := a.history.LatestMessageTime(ctx, Conversation(a.name, channel, ""))
		if err != nil {
			a.warnLookup(ctx, "read traQ backfill cursor", err, "channel_id", channel)
			continue
		}
		since := now.Add(-24 * time.Hour)
		if last.After(since) {
			since = last
		}
		targets = append(targets, backfillTarget{channel: channel, direct: channel == ownerChannel, since: since, until: now})
	}
	return targets
}

func (a *adapter) backfill(ctx context.Context, targets []backfillTarget, consume func(context.Context, domain.InboundEvent) error) error {
	for _, target := range targets {
		if ctx.Err() != nil {
			return errReconnect
		}
		messages, response, err := a.api.MessageAPI.GetMessages(ctx, target.channel).Since(target.since).Until(target.until).Inclusive(true).Limit(200).Order("desc").Execute()
		if err != nil {
			if ctx.Err() != nil {
				return errReconnect
			}
			a.warnLookup(ctx, "backfill traQ messages", apiError("backfill traQ messages", response, err), "channel_id", target.channel)
			continue
		}
		// The cap keeps the newest messages, then delivers that window chronologically.
		if len(messages) > 200 {
			messages = messages[:200]
		}
		slices.SortFunc(messages, func(a, b sdk.Message) int {
			if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
				return order
			}
			return cmp.Compare(a.Id, b.Id)
		})
		for _, message := range messages {
			if ctx.Err() != nil {
				return errReconnect
			}
			if message.UserId == a.self || !validID(message.UserId) || !validID(message.Id) || message.ChannelId != target.channel || message.CreatedAt.Before(target.since) || message.CreatedAt.After(target.until) {
				continue
			}
			user, err := a.lookupUser(ctx, message.UserId)
			if err != nil {
				if ctx.Err() != nil {
					return errReconnect
				}
				a.warnLookup(ctx, "read traQ user", err, "channel_id", target.channel, "message_id", message.Id, "user_id", message.UserId)
				continue
			}
			event := domain.InboundEvent{SourceKey: a.name + ":m:" + message.Id, Kind: domain.InboundEventMessage, Conversation: Conversation(a.name, target.channel, ""), MessageRef: Conversation(a.name, target.channel, message.Id), Direct: target.direct, Sender: user, Text: message.Content, MentionsMe: a.mentions(message.Content, nil), ReceivedAt: message.CreatedAt}
			if ctx.Err() != nil {
				return errReconnect
			}
			if err := consume(ctx, event); err != nil {
				return err
			}
		}
	}
	return nil
}
