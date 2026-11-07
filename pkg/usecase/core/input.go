package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (c *core) prepare(ctx context.Context, item domain.InboxItem) (domain.UserInput, error) {
	target, at, sender := item.Payload.Target, item.ReadyAt, ""
	if event := item.Payload.Event; event != nil {
		c.mu.Lock()
		_, accepted := c.accepted(*event)
		c.mu.Unlock()
		if !accepted {
			return domain.UserInput{}, fmt.Errorf("%w: conversation no longer accepts this sender", domain.ErrRejected)
		}
		target, at, sender = event.Conversation, event.ReceivedAt, event.Sender.ID
	}
	history, err := c.events.InputContext(ctx, item.SessionKey, target, at)
	if err != nil {
		return domain.UserInput{}, fmt.Errorf("%w: input context: %w", domain.ErrPersistence, err)
	}
	var info domain.SourceInfo
	owner := ""
	if target != "" && target != "owner" {
		instance, err := c.messaging.resolve(target)
		if err != nil {
			return domain.UserInput{}, err
		}
		owner = instance.config.Owner
		if provider, ok := instance.config.Adapter.(domain.SourceInfoProvider); ok {
			info = provider.SourceInfo(ctx, target, sender, !history.ThreadSeen && history.Root == nil)
		}
	}
	if history.Root != nil {
		info.ThreadText = history.Root.Text
		if info.ThreadAt.IsZero() && history.Root.Kind == domain.InboundEventMessage {
			info.ThreadAt = history.Root.ReceivedAt
		}
	}
	return formatInput(item, history, info, owner, time.Local), nil
}

func formatInput(item domain.InboxItem, history domain.InputContext, info domain.SourceInfo, owner string, location *time.Location) domain.UserInput {
	input := item.Payload.Input
	conversation, direct := item.Payload.Context.Conversation, item.Payload.Context.Direct
	reply, work := item.Payload.Target, item.Payload.Target
	at, sender := item.ReadyAt, "system"
	kind, mentioned := domain.InboundEventKind(""), false
	if prefix, _, ok := strings.Cut(item.SourceKey, ":"); ok && (prefix == "watch" || prefix == "wake") {
		sender = prefix
	}
	if event := item.Payload.Event; event != nil {
		conversation, direct = channel(*event), event.Direct
		reply, work = event.Conversation, event.MessageRef
		if event.Parent != "" || direct {
			work = reply
		}
		at, sender = event.ReceivedAt, event.Sender.DisplayName
		if sender == "" {
			sender = event.Sender.ID
		}
		if info.Sender != "" && (info.Sender != event.Sender.ID || event.Sender.DisplayName == "") {
			sender = info.Sender
		}
		if event.Sender.ID == owner {
			sender += " (owner)"
		}
		kind, mentioned = event.Kind, event.MentionsMe
		input = domain.UserInput{Text: event.Text, Attachments: event.Attachments}
	}
	where := info.Conversation
	if where == "" {
		where = string(conversation)
	}
	if direct {
		where = "DM with " + where
	}
	thread := "top level"
	if reply != "" && reply != conversation {
		thread = "thread"
		if !info.ThreadAt.IsZero() {
			thread += " " + info.ThreadAt.In(location).Format("15:04:05 MST")
		}
		if !history.ThreadSeen && info.ThreadText != "" {
			thread += fmt.Sprintf(" %q", excerpt(info.ThreadText))
		}
	}
	if item.Lane == "task" {
		where, thread = "Task "+item.Payload.Context.TaskName, ""
		reply, work = "owner", "owner"
	}
	if thread != "" {
		where += " · " + thread
	}
	if mentioned {
		sender += " → you"
	}
	header := fmt.Sprintf("%s | %s | %s\n", where, sender, at.In(location).Format("15:04:05 MST"))
	if kind == domain.InboundEventMessageEdited {
		header += "Message edited or deleted.\n"
	}
	if kind == domain.InboundEventReaction {
		header += "Reaction.\n"
	}
	if item.Payload.Event != nil {
		if history.LastReply.After(at) {
			place := "this conversation"
			if reply != conversation {
				place = "this thread"
			}
			header += fmt.Sprintf("Sent at %s, before your reply at %s in %s.\n", at.In(location).Format("15:04:05 MST"), history.LastReply.In(location).Format("15:04:05 MST"), place)
		} else if history.Running {
			header += "Sent while your previous turn was running. After its reply, answer only what this message adds.\n"
		}
	}
	input.Text = fmt.Sprintf("%sreply → %s | work → %s\n\n%s", header, reply, work, input.Text)
	return input
}

func excerpt(text string) string {
	chars := []rune(strings.Join(strings.Fields(text), " "))
	if len(chars) > 80 {
		return string(chars[:80]) + "…"
	}
	return string(chars)
}
