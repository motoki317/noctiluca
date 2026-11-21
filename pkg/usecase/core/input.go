package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (c *core) prepare(ctx context.Context, item domain.InboxItem) (domain.UserInput, error) {
	event := item.Payload.Event
	if event == nil {
		return formatInput(item, domain.InputContext{}, domain.SourceInfo{}, "", time.Local, c.config.Workdir), nil
	}
	c.mu.Lock()
	_, accepted := c.accepted(*event)
	c.mu.Unlock()
	if !accepted {
		return domain.UserInput{}, fmt.Errorf("%w: conversation no longer accepts this sender", domain.ErrRejected)
	}
	history, err := c.events.InputContext(ctx, domain.MainSessionKey, event.Conversation, event.ReceivedAt)
	if err != nil {
		return domain.UserInput{}, fmt.Errorf("%w: input context: %w", domain.ErrPersistence, err)
	}
	instance, err := c.messaging.resolve(event.Conversation)
	if err != nil {
		return domain.UserInput{}, err
	}
	var info domain.SourceInfo
	if provider, ok := instance.config.Adapter.(domain.SourceInfoProvider); ok {
		info = provider.SourceInfo(ctx, *event, history.Root == nil)
	}
	if history.Root != nil {
		info.ThreadText = history.Root.Text
		if info.ThreadAt.IsZero() && history.Root.Kind == domain.InboundEventMessage {
			info.ThreadAt = history.Root.ReceivedAt
		}
	}
	return formatInput(item, history, info, instance.config.Owner, time.Local, c.config.Workdir), nil
}

func formatInput(item domain.InboxItem, history domain.InputContext, info domain.SourceInfo, owner string, location *time.Location, workdir string) domain.UserInput {
	input := item.Payload.Input
	event := item.Payload.Event
	if event == nil {
		if prefix, _, ok := strings.Cut(item.SourceKey, ":"); ok && (prefix == "watch" || prefix == "wake") {
			return input
		}
		if item.Lane == domain.LaneTask {
			header := fmt.Sprintf("Task %s | system | %s\n", item.Payload.Context.TaskName, item.ReadyAt.In(location).Format("15:04:05 MST"))
			if hint := item.Payload.Context.Workdir; hint != "" && hint != workdir {
				header += "workdir: " + hint + "\n"
			}
			input.Text = header + "\n" + input.Text
		}
		return input
	}
	channelRef, conversation, direct := channel(*event), event.Conversation, event.Direct
	at, sender := event.ReceivedAt, event.Sender.DisplayName
	if sender == "" {
		sender = event.Sender.ID
	}
	if info.Sender != "" && (info.Sender != event.Sender.ID || event.Sender.DisplayName == "") {
		sender = info.Sender
	}
	if event.Sender.ID == owner {
		sender += " (owner)"
	}
	kind, mentioned := event.Kind, event.MentionsMe
	input = domain.UserInput{Text: event.Text, Attachments: event.Attachments}
	where := info.Conversation
	if where == "" {
		where = string(channelRef)
	}
	if direct {
		where = "DM with " + where
	}
	thread := "top level"
	if conversation != "" && conversation != channelRef {
		thread = "thread"
		if !info.ThreadAt.IsZero() {
			thread += " " + info.ThreadAt.In(location).Format("15:04:05 MST")
		}
		if history.Root != nil && history.Root.Deleted {
			thread += " (root deleted)"
		} else if info.ThreadText != "" {
			thread += fmt.Sprintf(" %q", excerpt(info.ThreadText))
		}
		if event.AlsoInChannel {
			thread += " (also sent to channel)"
		}
	}
	where += " · " + thread
	if mentioned {
		sender += " → you"
	}
	header := fmt.Sprintf("%s | %s | %s\n", where, sender, at.In(location).Format("15:04:05 MST"))
	if kind == domain.InboundEventMessageEdited {
		if item.Payload.Event.Deleted {
			header += "Message deleted.\n"
		} else {
			header += "Message edited.\n"
		}
	}
	if kind == domain.InboundEventReaction {
		header += "Reaction.\n"
	}
	if history.LastReply.After(at) {
		place := "this conversation"
		if conversation != channelRef {
			place = "this thread"
		}
		header += fmt.Sprintf("Sent at %s, before your reply at %s in %s.\n", at.In(location).Format("15:04:05 MST"), history.LastReply.In(location).Format("15:04:05 MST"), place)
	} else if history.Running {
		input.SentDuringTurn = true
	}
	if info.Location != "" {
		header += info.Location + "\n"
	}
	input.Text = header + "\n" + input.Text
	return input
}

func excerpt(text string) string {
	chars := []rune(strings.Join(strings.Fields(text), " "))
	if len(chars) > 80 {
		return string(chars[:80]) + "…"
	}
	return string(chars)
}
