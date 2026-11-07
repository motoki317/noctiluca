package discord

import (
	"context"
	"errors"
	"fmt"
	"time"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func New(name string, config Config, history domain.EventRepository, options ...rest.ClientConfigOpt) (domain.MessagingAdapter, error) {
	return newAdapter(name, config, history, options...)
}

func (a *adapter) Send(ctx context.Context, ref domain.ConversationRef, message domain.OutboundMessage) (string, error) {
	text := message.Text
	for _, attachment := range message.Attachments {
		file, err := a.file(ctx, attachment.Ref)
		if err != nil {
			return "", err
		}
		text += "\n<" + file.URL + ">"
	}
	created, err := a.post(ctx, ref, text, nil)
	if err != nil {
		return "", err
	}
	return created.ID.String(), nil
}

func (a *adapter) post(ctx context.Context, ref domain.ConversationRef, text string, files []*sdk.File) (*sdk.Message, error) {
	chunks, err := splitText(text)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		if len(files) == 0 {
			return nil, errors.New("message must contain text or a file")
		}
		chunks = []string{""}
	}
	target, err := a.sendTarget(ctx, ref)
	if err != nil {
		return nil, err
	}
	var created *sdk.Message
	for i, chunk := range chunks {
		var attachments []*sdk.File
		if i == len(chunks)-1 {
			attachments = files
		}
		created, err = a.api.CreateMessage(target, sdk.MessageCreate{Content: chunk, Files: attachments, AllowedMentions: &sdk.AllowedMentions{Parse: []sdk.AllowedMentionType{sdk.AllowedMentionTypeUsers}}}, rest.WithCtx(ctx))
		if err != nil {
			return nil, fmt.Errorf("%w (%d chunks already sent)", discordError("send Discord message failed", err), i)
		}
	}
	return created, nil
}

func (a *adapter) sendTarget(ctx context.Context, ref domain.ConversationRef) (snowflake.ID, error) {
	channel, thread, err := a.destination(ref)
	if err != nil {
		return 0, err
	}
	if thread == 0 {
		return channel, nil
	}
	existing, err := a.api.GetChannel(thread, rest.WithCtx(ctx))
	if err == nil {
		return threadTarget(existing, channel)
	}
	if !rest.IsJSONErrorCode(err, rest.JSONErrorCodeUnknownChannel) {
		return 0, discordError("read Discord thread failed", err)
	}
	root, err := a.api.GetMessage(channel, thread, rest.WithCtx(ctx))
	if err != nil {
		return 0, discordError("read Discord thread starter failed", err)
	}
	created, err := a.api.CreateThreadFromMessage(channel, thread, sdk.ThreadCreateFromMessage{Name: threadName(*root, snowflake.ID(a.self.Load()))}, rest.WithCtx(ctx))
	if rest.IsJSONErrorCode(err, rest.JSONErrorCodeThreadAlreadyCreatedForMessage) {
		existing, err = a.api.GetChannel(thread, rest.WithCtx(ctx))
		if err == nil {
			return threadTarget(existing, channel)
		}
	}
	if err != nil {
		return 0, discordError("start Discord thread failed", err)
	}
	return created.ID(), nil
}

func threadTarget(channel sdk.Channel, parent snowflake.ID) (snowflake.ID, error) {
	thread, ok := channel.(sdk.GuildThread)
	if !ok || thread.ParentID() == nil || *thread.ParentID() != parent {
		return 0, errors.New("thread does not belong to the Discord parent channel")
	}
	return thread.ID(), nil
}

func (a *adapter) record(ctx context.Context, id string, ref domain.ConversationRef, message domain.OutboundMessage) error {
	if err := a.history.SaveOutbound(ctx, domain.SentMessage{ID: id, Conversation: ref, Message: message, SentAt: time.Now()}); err != nil {
		return fmt.Errorf("message %s was sent but saving outbound history failed: %w", id, err)
	}
	return nil
}
