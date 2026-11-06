package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	sdk "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"golang.org/x/sync/errgroup"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/util/lifecycle"
)

type Config struct {
	AppToken  string `mapstructure:"app_token"`
	BotToken  string `mapstructure:"bot_token"`
	UserToken string `mapstructure:"user_token"`
}

type adapter struct {
	acceptConversation func(domain.ConversationRef, bool) bool
	life               lifecycle.Group
	bot, reader        *sdk.Client
	history            domain.EventRepository
	selfUser, selfBot  string
	socketEvents       <-chan socketmode.Event
	runSocket          func(context.Context) error
	ack                func(context.Context, string, any) error
}

func New(config Config, history domain.EventRepository, options ...sdk.Option) (domain.MessagingAdapter, error) {
	return newAdapter(config, history, options...)
}

func newAdapter(config Config, history domain.EventRepository, options ...sdk.Option) (*adapter, error) {
	if config.AppToken == "" || config.BotToken == "" {
		return nil, errors.New("app and bot tokens are required for Slack")
	}
	if history == nil {
		return nil, errors.New("history repository is required for Slack")
	}
	botOptions := append(append([]sdk.Option{}, options...), sdk.OptionAppLevelToken(config.AppToken))
	bot := sdk.New(config.BotToken, botOptions...)
	reader := bot
	if config.UserToken != "" {
		reader = sdk.New(config.UserToken, options...)
	}
	socket := socketmode.New(bot)
	return &adapter{bot: bot, reader: reader, history: history, socketEvents: socket.Events, runSocket: socket.RunContext, ack: socket.AckCtx}, nil
}

func (a *adapter) Start(ctx context.Context, consume func(context.Context, domain.InboundEvent) error) error {
	ctx, finish, beginErr := a.life.Begin(ctx)
	if beginErr != nil {
		return beginErr
	}
	defer finish()

	auth, err := a.bot.AuthTestContext(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("bot authentication failed for Slack")
	}
	a.selfUser, a.selfBot = auth.UserID, auth.BotID
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		defer cancel()
		err := a.runSocket(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return errors.New("socket mode connection failed for Slack")
		}
		return nil
	})
	g.Go(func() error {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return nil
			case e, ok := <-a.socketEvents:
				if !ok {
					return errors.New("socket mode stream closed for Slack")
				}
				if e.Type == socketmode.EventTypeEventsAPI {
					if payload, ok := e.Data.(slackevents.EventsAPIEvent); ok {
						if event, ok := a.inbound(ctx, payload.InnerEvent.Data); ok {
							if callback, ok := payload.Data.(*slackevents.EventsAPICallbackEvent); ok {
								event.SourceKey = "slack:" + callback.EventID
							}
							if event.SourceKey == "" || event.SourceKey == "slack:" {
								if e.Request != nil {
									event.SourceKey = "slack:" + e.Request.EnvelopeID
								}
							}
							if err := consume(ctx, event); err != nil {
								return err
							}
						}
					}
				}
				if e.Request != nil && e.Request.EnvelopeID != "" {
					ackCtx, done := context.WithTimeout(ctx, 3*time.Second)
					err := a.ack(ackCtx, e.Request.EnvelopeID, nil)
					done()
					if err != nil {
						slog.WarnContext(ctx, "Slack acknowledgment failed", "error", err)
					}
				}
			}
		}
	})
	return g.Wait()
}

func (a *adapter) UserGroups(ctx context.Context, ids []string) (map[string][]string, error) {
	groups := make(map[string][]string, len(ids))
	for _, id := range ids {
		// The optional read token need not have the usergroups:read scope.
		members, err := a.bot.GetUserGroupMembersContext(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("read Slack user group %q failed", id)
		}
		groups[id] = members
	}
	return groups, nil
}

func Conversation(channel, thread string) domain.ConversationRef {
	return domain.ConversationRef("slack:" + channel + ":" + thread)
}

func destination(ref domain.ConversationRef) (string, string, error) {
	parts := strings.Split(string(ref), ":")
	if len(parts) != 3 || parts[0] != "slack" || parts[1] == "" || strings.ContainsAny(parts[1], " \t\n\r") {
		return "", "", errors.New("invalid Slack conversation reference")
	}
	return parts[1], parts[2], nil
}

func (a *adapter) Send(ctx context.Context, ref domain.ConversationRef, message domain.OutboundMessage) (string, error) {
	return a.post(ctx, ref, message, false)
}

func (a *adapter) post(ctx context.Context, ref domain.ConversationRef, message domain.OutboundMessage, broadcast bool) (string, error) {
	channel, thread, err := destination(ref)
	if err != nil {
		return "", err
	}
	if broadcast && thread == "" {
		return "", errors.New("broadcast requires a thread")
	}
	text := mrkdwn(message.Text)
	for _, attachment := range message.Attachments {
		file, _, _, err := a.reader.GetFileInfoContext(ctx, attachment.Ref, 0, 0)
		if err != nil {
			return "", slackError("read Slack file failed", err)
		}
		if file.Permalink == "" {
			return "", errors.New("file has no Slack permalink")
		}
		text += "\n<" + file.Permalink + "|" + escape(file.Name) + ">"
	}
	options := []sdk.MsgOption{sdk.MsgOptionText(text, false)}
	if thread != "" {
		options = append(options, sdk.MsgOptionTS(thread))
	}
	if broadcast {
		options = append(options, sdk.MsgOptionBroadcast())
	}
	_, id, err := a.bot.PostMessageContext(ctx, channel, options...)
	if err != nil {
		return "", slackError("post Slack message failed", err)
	}
	return id, nil
}

func (a *adapter) Shutdown(ctx context.Context) error { return a.life.Shutdown(ctx) }

func (a *adapter) DirectMessage(ctx context.Context, user string) (domain.ConversationRef, error) {
	channel, _, _, err := a.bot.OpenConversationContext(ctx, &sdk.OpenConversationParameters{Users: []string{user}})
	if err != nil {
		return "", errors.New("open owner direct message failed")
	}
	return Conversation(channel.ID, ""), nil
}

func (a *adapter) SetConversationFilter(accept func(domain.ConversationRef, bool) bool) {
	a.acceptConversation = accept
}
