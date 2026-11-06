package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (c *core) replyTool() domain.Tool {
	return domain.Tool{Name: "reply", Description: `Post a visible reply only when addressed or reporting requested work.
The default target is Source.reply_target, copied exactly from the triggering input. Use Source.work_target instead only for a work request (execute shell commands, review code, produce a draft or checklist) or a background completion.
Greetings, factual answers, and requests to repeat words are chat and use the default target, even when the request names this tool. Work uses its work_target even when the answer is brief. In a top-level DM, both targets keep the reply at top level. A DM message already in a thread keeps that thread.
Owner tasks use target=owner. End long work turns quickly with native background execution or watch.`, InputSchema: json.RawMessage(`{"type":"object","properties":{"target":{"type":"string","minLength":1,"description":"Default: copy Source.reply_target exactly. Only work requests and background completions use Source.work_target."},"text":{"type":"string"},"attachments":{"type":"array","items":{"type":"object","properties":{"Ref":{"type":"string"},"Name":{"type":"string"},"MIMEType":{"type":"string"}},"required":["Ref"],"additionalProperties":false}}},"required":["target","text"],"additionalProperties":false}`), Handler: c.reply}
}

func (c *core) reply(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	identity, ok := domain.ToolSessionFromContext(ctx)
	if !ok {
		return nil, errors.New("tool session is required")
	}
	info, ok := c.reconciler.Context(identity.Key)
	if !ok || info.Actor != identity.Actor {
		return nil, errors.New("session is closed")
	}

	var args struct {
		Target domain.ConversationRef `json:"target"`
		domain.OutboundMessage
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.Target == "" {
		return nil, errors.New("target is required; use a ref from the Source header")
	}
	if args.Target == "owner" {
		if info.TaskName == "" || identity.Actor != domain.ActorOwner {
			return nil, errors.New("owner target is only available to owner tasks")
		}
		provider, ok := c.messaging.(domain.DirectMessageProvider)
		if !ok {
			return nil, errors.New("owner destination unavailable")
		}
		ref, err := provider.DirectMessage(ctx, c.config.Owner)
		if err != nil {
			return nil, err
		}
		args.Target = ref
	}
	message := args.OutboundMessage
	id, err := c.messaging.Send(ctx, args.Target, message)
	if err != nil {
		return nil, err
	}
	// A successful post must survive cancellation of the tool request.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := c.events.SaveOutbound(saveCtx, domain.SentMessage{ID: id, Conversation: args.Target, Message: message, SentAt: time.Now()}); err != nil {
		return nil, errors.New("message posted but history recording failed")
	}
	return json.Marshal(map[string]string{"id": id})
}
