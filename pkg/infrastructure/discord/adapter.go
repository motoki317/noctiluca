package discord

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/util/lifecycle"
)

type Config struct {
	Token      string
	Owner      string
	NeedGroups bool
}

type adapter struct {
	name               string
	config             Config
	history            domain.EventRepository
	client             rest.Client
	api                rest.Rest
	life               lifecycle.Group
	started            atomic.Bool
	failures           int
	locationsMu        sync.Mutex
	locations          map[snowflake.ID]channelLocation
	joinedThreads      map[snowflake.ID]bool
	self               atomic.Uint64
	closeOnce          sync.Once
	createGateway      gateway.CreateFunc
	gatewayOptions     []gateway.ConfigOpt
	acceptConversation func(domain.ConversationRef, bool) bool
}

func newAdapter(name string, config Config, history domain.EventRepository, options ...rest.ClientConfigOpt) (*adapter, error) {
	if !domain.ValidMessagingName(name) {
		return nil, errors.New("invalid messaging entry name")
	}
	if config.Token == "" || history == nil {
		return nil, errors.New("token and history repository required for Discord")
	}
	if _, err := parseID(config.Owner); err != nil {
		return nil, errors.New("invalid Discord owner ID")
	}
	logger := disgoLogger()
	options = append(options, rest.WithLogger(logger), rest.WithRateLimiterConfigOpts(rest.WithRateLimiterLogger(logger)))
	client := rest.NewClient(config.Token, options...)
	return &adapter{name: name, config: config, history: history, client: client, api: rest.New(client), createGateway: gateway.New,
		locations: make(map[snowflake.ID]channelLocation), joinedThreads: make(map[snowflake.ID]bool)}, nil
}

func (a *adapter) Start(ctx context.Context, consume func(context.Context, domain.InboundEvent) error) (result error) {
	ctx, finish, err := a.life.Begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if !a.started.CompareAndSwap(false, true) {
		return errors.New("adapter already started for Discord")
	}
	defer a.started.Store(false)
	lifetimeCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if a.failures > 0 {
		delay := min(time.Second<<min(a.failures-1, 9), 5*time.Minute)
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	defer func() {
		if result != nil && lifetimeCtx.Err() == nil {
			a.failures = min(a.failures+1, 10)
		}
	}()
	var connectedAt atomic.Pointer[time.Time]
	connected := make(chan struct{}, 1)
	events := eventQueue{ready: make(chan struct{}, 1)}
	closed := make(chan struct{}, 1)
	options := []gateway.ConfigOpt{gateway.WithIntents(gateway.IntentGuilds, gateway.IntentGuildMessages, gateway.IntentGuildMessageReactions,
		gateway.IntentDirectMessages, gateway.IntentDirectMessageReactions, gateway.IntentMessageContent),
		gateway.WithCloseHandler(func(_ gateway.Gateway, _ error, reconnect bool) {
			if !reconnect {
				select {
				case closed <- struct{}{}:
				default:
				}
			}
		})}
	if a.config.NeedGroups {
		options = append(options, gateway.WithIntents(gateway.IntentGuildMembers))
	}
	options = append(options, a.gatewayOptions...)
	logger := disgoLogger()
	options = append(options, gateway.WithLogger(logger), gateway.WithRateLimiterConfigOpts(gateway.WithRateLimiterLogger(logger)))
	g := a.createGateway(a.config.Token, func(_ gateway.Gateway, _ gateway.EventType, _ int, event gateway.EventData) {
		if ctx.Err() != nil {
			return
		}
		switch event.(type) {
		case gateway.EventReady, gateway.EventResumed:
			at := time.Now()
			connectedAt.Store(&at)
			select {
			case connected <- struct{}{}:
			default:
			}
		}
		switch event.(type) {
		case gateway.EventReady, gateway.EventResumed, gateway.EventMessageCreate, gateway.EventMessageUpdate, gateway.EventMessageDelete, gateway.EventMessageDeleteBulk, gateway.EventMessageReactionAdd:
			events.push(ctx, event)
		}
	}, options...)
	gatewayClosed := make(chan struct{})
	closeGateway := func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		// Disgo may reconnect after Close (disgoorg/disgo#648).
		// The handler's `ctx.Err()` check drops events from such a connection.
		g.Close(closeCtx)
		close(gatewayClosed)
	}
	defer func() { cancel(); <-gatewayClosed }()
	if err := g.Open(ctx); err != nil {
		closeGateway()
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("connect Discord gateway failed")
	}
	at := time.Now()
	connectedAt.CompareAndSwap(nil, &at)
	context.AfterFunc(ctx, closeGateway)
	observed := make(chan struct{})
	go func() {
		defer close(observed)
		a.resetFailuresWhenStable(ctx, g, &connectedAt, connected)
	}()
	defer func() { cancel(); <-observed }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-closed:
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("gateway closed for Discord")
		case <-events.ready:
			for _, event := range events.take() {
				if ctx.Err() != nil {
					return nil
				}
				if err := a.dispatch(ctx, event, consume); err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
			}
		}
	}
}

// a.failures has no lock, so Start must join this observer before recording another failed attempt.
func (a *adapter) resetFailuresWhenStable(ctx context.Context, g gateway.Gateway, connectedAt *atomic.Pointer[time.Time], connected <-chan struct{}) {
	const stableFor = 10 * time.Minute
	timer := time.NewTimer(max(0, stableFor-time.Since(*connectedAt.Load())))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-connected:
		case <-timer.C:
			at := connectedAt.Load()
			// Disgo handles reconnect outages internally, without calling the close handler.
			if time.Since(*at) >= stableFor && g.Status() == gateway.StatusReady && connectedAt.Load() == at {
				a.failures = 0
				return
			}
		}
		if remaining := stableFor - time.Since(*connectedAt.Load()); remaining > 0 {
			timer.Reset(remaining)
		}
	}
}

func (a *adapter) Shutdown(ctx context.Context) error {
	if err := a.life.Shutdown(ctx); err != nil {
		return err
	}
	a.closeOnce.Do(func() { a.client.Close(ctx) })
	return nil
}

func Conversation(name, channel, thread string) domain.ConversationRef {
	return domain.NewConversationRef(name, channel, thread)
}

func parseID(value string) (snowflake.ID, error) {
	id, err := snowflake.Parse(value)
	if err != nil || id == 0 || id.String() != value {
		return 0, errors.New("expected a Discord numeric ID")
	}
	return id, nil
}

func (a *adapter) destination(ref domain.ConversationRef) (snowflake.ID, snowflake.ID, error) {
	name, channelID, threadID, ok := domain.ParseConversationRef(ref)
	if !ok || name != a.name {
		return 0, 0, errors.New("invalid Discord conversation reference")
	}
	channel, err := parseID(channelID)
	if err != nil {
		return 0, 0, err
	}
	var thread snowflake.ID
	if threadID != "" {
		thread, err = parseID(threadID)
	}
	return channel, thread, err
}

func discordError(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var response *rest.Error
	if errors.As(err, &response) && response.Response != nil {
		return fmt.Errorf("%s: Discord HTTP %d", operation, response.Response.StatusCode)
	}
	return errors.New(operation)
}

func (a *adapter) SetConversationFilter(accept func(domain.ConversationRef, bool) bool) {
	a.acceptConversation = accept
}

func (a *adapter) DirectMessage(ctx context.Context, user string) (domain.ConversationRef, error) {
	id, err := parseID(user)
	if err != nil {
		return "", err
	}
	channel, err := a.api.CreateDMChannel(id, rest.WithCtx(ctx))
	if err != nil {
		return "", discordError("open Discord DM failed", err)
	}
	return Conversation(a.name, channel.ID().String(), ""), nil
}

func sender(user sdk.User, member *sdk.Member) domain.Sender {
	name := user.EffectiveName()
	if member != nil && member.Nick != nil && *member.Nick != "" {
		name = *member.Nick
	}
	kind := domain.SenderHuman
	if user.Bot {
		kind = domain.SenderBot
	}
	return domain.Sender{ID: user.ID.String(), DisplayName: name, Kind: kind}
}
