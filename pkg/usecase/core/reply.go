package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (c *core) replyTool() domain.Tool {
	return domain.Tool{Name: "reply", Description: `Post only when addressed or reporting requested work. Copy the input's reply target for chat and factual answers. Copy its work target for shell commands, reviews, drafts, and background completions. Both targets preserve existing threads and keep top-level DMs at top level.
If the message arrived before your last reply, answer only what it adds. Do not repeat your previous reply. If a follow-up asks to append a phrase, send only that phrase once the answer is posted.`, InputSchema: json.RawMessage(`{"type":"object","properties":{"target":{"type":"string","minLength":1,"description":"Copy the reply target for chat, or the work target for work and background completions."},"text":{"type":"string"},"attachments":{"type":"array","items":{"type":"object","properties":{"Ref":{"type":"string"},"Name":{"type":"string"},"MIMEType":{"type":"string"}},"required":["Ref"],"additionalProperties":false}}},"required":["target","text"],"additionalProperties":false}`), Handler: c.reply}
}

func (c *core) reply(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	identity, ok := domain.ToolSessionFromContext(ctx)
	if !ok {
		return nil, errors.New("tool session is required")
	}
	_, ok = c.reconciler.Context(identity.Key)
	if !ok {
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
		return nil, errors.New("target is required; copy a target from the input")
	}
	if args.Target == "owner" {
		first := c.messaging.instances[0]
		provider, ok := first.config.Adapter.(domain.DirectMessageProvider)
		if !ok {
			return nil, errors.New("owner destination unavailable")
		}
		ref, err := provider.DirectMessage(ctx, first.config.Owner)
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
