package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	sdk "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/motoki317/noctiluca/pkg/domain"
)

const backfillLimit = 200

type backfillTarget struct {
	channel, thread string
	since, until    time.Time
}

func (a *adapter) backfillTargets(ctx context.Context) []backfillTarget {
	now := time.Now()
	since := now.Add(-24 * time.Hour)
	conversations, err := a.history.ListConversations(ctx, a.name)
	if err != nil {
		slog.WarnContext(ctx, "List Slack backfill conversations failed", "error", err)
	}
	refs := make([]domain.ConversationRef, 0, len(conversations))
	if a.owner != "" {
		ref, err := a.DirectMessage(ctx, a.owner)
		if err != nil {
			slog.WarnContext(ctx, "Resolve Slack backfill owner DM failed", "error", err)
		} else {
			refs = append(refs, ref)
		}
	}
	for _, conversation := range conversations {
		refs = append(refs, conversation.Ref)
	}
	threads, err := a.history.ListThreads(ctx, a.name, since)
	if err != nil {
		slog.WarnContext(ctx, "List Slack backfill threads failed", "error", err)
	}
	refs = append(refs, threads...)
	var targets []backfillTarget
	seen := make(map[domain.ConversationRef]bool)
	for _, ref := range refs {
		if ctx.Err() != nil {
			break
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		channel, thread, err := a.destination(ref)
		if err != nil {
			slog.WarnContext(ctx, "Resolve Slack backfill conversation failed", "conversation", ref, "error", err)
			continue
		}
		last, err := a.history.LatestMessageTime(ctx, ref)
		if err != nil {
			slog.WarnContext(ctx, "Read Slack backfill cursor failed", "conversation", ref, "error", err)
			continue
		}
		if thread != "" {
			sent, err := a.history.ListOutbound(ctx, ref, 1)
			if err != nil {
				slog.WarnContext(ctx, "Read Slack backfill cursor failed", "conversation", ref, "error", err)
				continue
			}
			if len(sent) > 0 && sent[0].SentAt.After(last) {
				last = sent[0].SentAt
			}
		}
		start := since
		if last.After(start) {
			start = last
		}
		targets = append(targets, backfillTarget{channel: channel, thread: thread, since: start, until: now})
	}
	return targets
}

func (a *adapter) backfill(ctx context.Context, targets []backfillTarget, consume func(context.Context, domain.InboundEvent) error) error {
	for _, target := range targets {
		messages, err := a.readBackfill(ctx, target)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.WarnContext(ctx, "Slack backfill read failed", "channel", target.channel, "thread", target.thread, "error", slackError("read messages", err))
			continue
		}
		for _, message := range messages {
			if ctx.Err() != nil {
				return nil
			}
			event, ok := a.inbound(ctx, &slackevents.MessageEvent{
				Channel: target.channel, SubType: message.SubType, Message: &message.Msg, PreviousMessage: message.PreviousMessage,
			})
			if ok && event.Kind == domain.InboundEventMessage {
				if err := consume(ctx, event); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func formatTimestamp(at time.Time) string {
	return fmt.Sprintf("%d.%06d", at.Unix(), at.Nanosecond()/int(time.Microsecond))
}

func (a *adapter) readBackfill(ctx context.Context, target backfillTarget) ([]sdk.Message, error) {
	oldest, latest := formatTimestamp(target.since), formatTimestamp(target.until)
	reader := a.bot
	var cursor string
	var messages []sdk.Message
	for {
		var page []sdk.Message
		var next string
		var err error
		if target.thread == "" {
			var result *sdk.GetConversationHistoryResponse
			result, err = reader.GetConversationHistoryContext(ctx, &sdk.GetConversationHistoryParameters{
				ChannelID: target.channel, Oldest: oldest, Latest: latest, Limit: backfillLimit - len(messages), Cursor: cursor,
			})
			if err == nil {
				page, next = result.Messages, result.ResponseMetaData.NextCursor
			}
		} else {
			page, _, next, err = reader.GetConversationRepliesContext(ctx, &sdk.GetConversationRepliesParameters{
				ChannelID: target.channel, Timestamp: target.thread, Oldest: oldest, Latest: latest, Limit: backfillLimit, Cursor: cursor,
			})
		}
		if reader == a.bot && a.fallbackReader(err) {
			reader = a.reader
			continue
		}
		var limited *sdk.RateLimitedError
		if errors.As(err, &limited) {
			timer := time.NewTimer(limited.RetryAfter)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		for _, message := range page {
			at := timestamp(message.Timestamp)
			if !at.After(target.since) || at.After(target.until) || (target.thread != "" && message.Timestamp == target.thread) {
				continue
			}
			messages = append(messages, message)
		}
		if target.thread == "" && len(messages) >= backfillLimit {
			messages = messages[:backfillLimit]
			break
		}
		// Replies arrive oldest first, so later pages can replace the entire retained window.
		if len(messages) > backfillLimit {
			messages = messages[len(messages)-backfillLimit:]
		}
		cursor = next
		if cursor == "" {
			break
		}
	}
	slices.SortFunc(messages, func(a, b sdk.Message) int { return timestamp(a.Timestamp).Compare(timestamp(b.Timestamp)) })
	return messages, nil
}
