package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
	name               string
	names              sync.Map
	joinedThreads      sync.Map
	acceptConversation func(domain.ConversationRef) bool
	life               lifecycle.Group
	bot, reader        *sdk.Client
	history            domain.EventRepository
	selfUser, selfBot  string
	socketEvents       <-chan socketmode.Event
	runSocket          func(context.Context) error
	ack                func(context.Context, string, any) error
}

func New(name string, config Config, history domain.EventRepository, options ...sdk.Option) (domain.MessagingAdapter, error) {
	return newAdapter(name, config, history, options...)
}

func newAdapter(name string, config Config, history domain.EventRepository, options ...sdk.Option) (*adapter, error) {
	if !domain.ValidMessagingName(name) {
		return nil, errors.New("invalid messaging entry name")
	}
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
	return &adapter{name: name, bot: bot, reader: reader, history: history, socketEvents: socket.Events, runSocket: socket.RunContext, ack: socket.AckCtx}, nil
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
								event.SourceKey = a.name + ":" + callback.EventID
							}
							if event.SourceKey == "" || event.SourceKey == a.name+":" {
								if e.Request != nil {
									event.SourceKey = a.name + ":" + e.Request.EnvelopeID
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

func Conversation(name, channel, thread string) domain.ConversationRef {
	return domain.NewConversationRef(name, channel, thread)
}

func (a *adapter) PostRefs(conversation, message domain.ConversationRef, direct bool) []domain.NamedRef {
	_, thread, err := a.destination(conversation)
	if err != nil || direct || message == "" || thread != "" {
		return nil
	}
	return []domain.NamedRef{{Name: "thread", Ref: message}}
}

func (a *adapter) destination(ref domain.ConversationRef) (string, string, error) {
	name, channel, thread, ok := domain.ParseConversationRef(ref)
	if !ok || name != a.name || channel == "" || strings.ContainsAny(channel, " \t\n\r") {
		return "", "", errors.New("invalid Slack conversation reference")
	}
	return channel, thread, nil
}

func (a *adapter) Send(ctx context.Context, ref domain.ConversationRef, message domain.OutboundMessage) (string, error) {
	return a.post(ctx, ref, message, false)
}

func (a *adapter) post(ctx context.Context, ref domain.ConversationRef, message domain.OutboundMessage, broadcast bool) (string, error) {
	channel, thread, err := a.destination(ref)
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
	return Conversation(a.name, channel.ID, ""), nil
}

func (a *adapter) SetConversationFilter(accept func(domain.ConversationRef) bool) {
	a.acceptConversation = accept
}
