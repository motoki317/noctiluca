package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (c *core) replyTool() domain.Tool {
	return domain.Tool{Name: "reply", Description: "Post a message to the current conversation.", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"attachments":{"type":"array","items":{"type":"object","properties":{"Ref":{"type":"string"},"Name":{"type":"string"},"MIMEType":{"type":"string"}},"required":["Ref"],"additionalProperties":false}}},"required":["text"],"additionalProperties":false}`), Handler: c.reply}
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

	var message domain.OutboundMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, err
	}
	if info.Conversation == "" {
		if err := c.notifyOwner(ctx, message.Text); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"sent":true}`), nil
	}
	id, err := c.messaging.Send(ctx, info.Conversation, message)
	if err != nil {
		return nil, err
	}
	// A successful post must survive cancellation of the tool request.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := c.events.SaveOutbound(saveCtx, domain.SentMessage{ID: id, Conversation: info.Conversation, Message: message, SentAt: time.Now()}); err != nil {
		return nil, errors.New("message posted but history recording failed")
	}
	return json.Marshal(map[string]string{"id": id})
}
