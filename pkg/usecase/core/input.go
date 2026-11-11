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
	message, direct := item.Payload.MessageRef, item.Payload.Context.Direct
	if event := item.Payload.Event; event != nil {
		c.mu.Lock()
		_, accepted := c.accepted(*event)
		c.mu.Unlock()
		if !accepted {
			return domain.UserInput{}, fmt.Errorf("%w: conversation no longer accepts this sender", domain.ErrRejected)
		}
		target, at, sender = event.Conversation, event.ReceivedAt, event.Sender.ID
		message, direct = event.MessageRef, event.Direct
	}
	history, err := c.events.InputContext(ctx, item.SessionKey, target, at)
	if err != nil {
		return domain.UserInput{}, fmt.Errorf("%w: input context: %w", domain.ErrPersistence, err)
	}
	var info domain.SourceInfo
	var refs []domain.NamedRef
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
		if provider, ok := instance.config.Adapter.(domain.PostRefProvider); ok && item.Lane != "task" {
			refs = provider.PostRefs(target, message, direct)
		}
	}
	if history.Root != nil {
		info.ThreadText = history.Root.Text
		if info.ThreadAt.IsZero() && history.Root.Kind == domain.InboundEventMessage {
			info.ThreadAt = history.Root.ReceivedAt
		}
	}
	return formatInput(item, history, info, refs, owner, time.Local), nil
}

func formatInput(item domain.InboxItem, history domain.InputContext, info domain.SourceInfo, refs []domain.NamedRef, owner string, location *time.Location) domain.UserInput {
	input := item.Payload.Input
	channelRef, direct := item.Payload.Context.Conversation, item.Payload.Context.Direct
	conversation := item.Payload.Target
	at, sender := item.ReadyAt, "system"
	kind, mentioned := domain.InboundEventKind(""), false
	if prefix, _, ok := strings.Cut(item.SourceKey, ":"); ok && (prefix == "watch" || prefix == "wake") {
		sender = prefix
	}
	if event := item.Payload.Event; event != nil {
		channelRef, direct = channel(*event), event.Direct
		conversation = event.Conversation
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
		if !history.ThreadSeen {
			if history.Root != nil && history.Root.Deleted {
				thread += " (root deleted)"
			} else if info.ThreadText != "" {
				thread += fmt.Sprintf(" %q", excerpt(info.ThreadText))
			}
		}
	}
	if item.Lane == "task" {
		where, thread = "Task "+item.Payload.Context.TaskName, ""
		conversation, refs = "owner", nil
	}
	if thread != "" {
		where += " · " + thread
	}
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
	if event := item.Payload.Event; event != nil {
		if history.LastReply.After(at) {
			place := "this conversation"
			if conversation != channelRef {
				place = "this thread"
			}
			header += fmt.Sprintf("Sent at %s, before your reply at %s in %s.\n", at.In(location).Format("15:04:05 MST"), history.LastReply.In(location).Format("15:04:05 MST"), place)
		} else if history.Running {
			header += "Sent while your previous turn was running. After its reply, answer only what this message adds.\n"
		}
		if kind == domain.InboundEventMessage || kind == domain.InboundEventMessageEdited {
			header += fmt.Sprintf("message: %s\n", event.MessageRef)
		}
	}
	header += fmt.Sprintf("refs: conversation=%s", conversation)
	for _, ref := range refs {
		header += fmt.Sprintf(" %s=%s", ref.Name, ref.Ref)
	}
	input.Text = header + "\n\n" + input.Text
	return input
}

func excerpt(text string) string {
	chars := []rune(strings.Join(strings.Fields(text), " "))
	if len(chars) > 80 {
		return string(chars[:80]) + "…"
	}
	return string(chars)
}
