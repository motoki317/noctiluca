package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/motoki317/noctiluca/pkg/domain"
	memoryusecase "github.com/motoki317/noctiluca/pkg/usecase/memory"
	"github.com/motoki317/noctiluca/pkg/usecase/reconcile"
	"github.com/motoki317/noctiluca/pkg/usecase/watch"
	"github.com/motoki317/noctiluca/pkg/usecase/webhook"
	"golang.org/x/sync/errgroup"
)

type Config struct {
	RetryDelay   time.Duration
	MaxAttempts  int
	Workdir      string
	Jobs         []domain.Schedule
	Procedure    func(context.Context, string) (string, error)
	EventsListen string
	EventsToken  string
}
type rule struct {
	users, bots            map[string]struct{}
	anySender              bool
	ignore, requireMention bool
}
type core struct {
	config           Config
	messaging        *messagingRouter
	events           domain.EventRepository
	inbox            domain.InboxRepository
	hub              domain.ToolHub
	reconciler       *reconcile.Reconciler
	watcher          domain.Background
	webhook          domain.Background
	external         domain.ExternalTools
	mu               sync.Mutex
	cancel           context.CancelFunc
	started, stopped bool
	done             chan struct{}
}

func New(config Config, messaging []Messaging, model domain.ModelAdapter, hub domain.ToolHub, events domain.EventRepository, sessions domain.SessionRepository, prompt domain.PromptAssembler, schedules domain.ScheduleRepository, watches domain.WatchRepository, external domain.ExternalTools, memory domain.MemoryStore, inbox domain.InboxRepository) (domain.Core, error) {
	if model == nil || hub == nil || events == nil || sessions == nil || schedules == nil || inbox == nil {
		return nil, errors.New("core dependencies are required")
	}
	router, err := newMessagingRouter(messaging)
	if err != nil {
		return nil, err
	}
	c := &core{config: config, messaging: router, events: events, inbox: inbox, hub: hub, external: external, done: make(chan struct{})}
	for _, instance := range router.instances {
		if err := hub.AddSource("messaging:"+instance.config.Name, instance.config.Adapter.Tools()); err != nil {
			return nil, err
		}
		if filter, ok := instance.config.Adapter.(domain.ConversationFilter); ok {
			filter.SetConversationFilter(func(ref domain.ConversationRef) bool {
				c.mu.Lock()
				defer c.mu.Unlock()
				owner, err := router.resolve(ref)
				if err != nil || owner != instance {
					return false
				}
				_, ok := c.findRule(ref)
				return ok
			})
		}
	}
	c.reconciler = reconcile.New(reconcile.Config{Workdir: config.Workdir, RetryDelay: config.RetryDelay, MaxAttempts: config.MaxAttempts, Prepare: c.prepare, Prompt: func(ctx context.Context, session domain.ToolSession) (string, error) {
		return memoryusecase.Prompt(memory, c.toolNotes(ctx), prompt)(ctx, session)
	}, Accept: c.acceptSession, Procedure: config.Procedure, NotifyOwner: c.notifyOwner}, inbox, schedules, sessions, events, model, hub)
	c.watcher = watch.New(watches, inbox, c.reconciler.Notify)
	tools := c.reconciler.Tools()
	tools = append(tools, c.watcher.Tools()...)
	if err := hub.AddSource("core", tools); err != nil {
		return nil, err
	}
	if config.EventsListen != "" {
		c.webhook, err = webhook.New(config.EventsListen, config.EventsToken, inbox, c.reconciler.Notify)
		if err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *core) Start(ctx context.Context) (result error) {
	c.mu.Lock()
	if c.started || c.stopped {
		c.mu.Unlock()
		return errors.New("core is stopped or already started")
	}
	c.started = true
	ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()
	defer close(c.done)
	defer func() {
		c.cancel()
		c.mu.Lock()
		c.stopped = true
		c.mu.Unlock()
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result = errors.Join(result, c.shutdownServices(stop))
		if ctx.Err() != nil && errors.Is(result, ctx.Err()) {
			result = nil
		}
	}()
	if c.external != nil {
		if err := c.external.Connect(ctx); err != nil {
			slog.WarnContext(ctx, "External tools unavailable", "error", err)
		}
	}
	var initialGroups sync.WaitGroup
	for _, instance := range c.messaging.instances {
		if provider, ok := instance.config.Adapter.(domain.UserGroupProvider); ok && len(instance.groupIDs) > 0 {
			initialGroups.Go(func() { c.loadGroups(ctx, instance, provider) })
		}
	}
	initialGroups.Wait()
	if err := c.reconciler.Install(ctx, c.config.Jobs); err != nil {
		return err
	}
	g, runCtx := errgroup.WithContext(ctx)
	g.Go(func() error { return c.hub.Start(runCtx) })
	g.Go(func() error { return c.reconciler.Start(runCtx) })
	g.Go(func() error { return c.watcher.Start(runCtx) })
	if c.webhook != nil {
		g.Go(func() error { return c.webhook.Start(runCtx) })
	}
	for _, instance := range c.messaging.instances {
		g.Go(func() error { return c.runMessaging(runCtx, instance) })
		if provider, ok := instance.config.Adapter.(domain.UserGroupProvider); ok && len(instance.groupIDs) > 0 {
			g.Go(func() error { return c.refreshGroups(runCtx, instance, provider) })
		}
	}
	if c.external != nil {
		g.Go(func() error { return c.external.Start(runCtx, c.reconciler.Notify) })
	}
	result = g.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return result
}

func (c *core) shutdownServices(ctx context.Context) error {
	err := errors.Join(c.hub.Shutdown(ctx), c.messaging.Shutdown(ctx), c.watcher.Shutdown(ctx))
	if c.webhook != nil {
		err = errors.Join(err, c.webhook.Shutdown(ctx))
	}
	if c.external != nil {
		err = errors.Join(err, c.external.Shutdown(ctx))
	}
	return err
}

func (c *core) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	if !c.started {
		c.stopped = true
		c.mu.Unlock()
		return c.shutdownServices(ctx)
	}
	c.cancel()
	c.mu.Unlock()
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *core) accepted(event domain.InboundEvent) (rule, bool) {
	r, ok := c.findRule(channel(event))
	if !ok || event.Conversation == "" || (r.requireMention && !event.MentionsMe) {
		return r, false
	}
	switch event.Sender.Kind {
	case domain.SenderHuman:
		_, ok = r.users[event.Sender.ID]
	case domain.SenderBot:
		_, ok = r.bots[event.Sender.ID]
	default:
		return r, false
	}
	return r, ok || r.anySender
}
func (c *core) receive(ctx context.Context, event domain.InboundEvent) error {
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = time.Now()
	}
	if event.SourceKey == "" {
		event.SourceKey = uuid.NewString()
	}
	c.mu.Lock()
	_, accepted := c.accepted(event)
	c.mu.Unlock()
	event.Accepted = &accepted
	item := domain.InboxItem{SourceKey: event.SourceKey, SessionKey: domain.MainSessionKey, Lane: domain.LaneConversation, ReadyAt: event.ReceivedAt, Payload: domain.InboxPayload{Event: &event, Context: domain.SessionContext{Conversation: channel(event), Direct: event.Direct, Workdir: c.config.Workdir}}}
	if !accepted {
		item.Status = domain.InboxDone
	}
	if err := c.inbox.Receive(ctx, event, item); err != nil {
		return errors.Join(domain.ErrPersistence, err)
	}
	c.reconciler.Notify()
	return nil
}
func channel(event domain.InboundEvent) domain.ConversationRef {
	if event.Parent != "" {
		return event.Parent
	}
	return event.Conversation
}

func (c *core) notifyOwner(ctx context.Context, text string) error {
	first := c.messaging.instances[0]
	provider, ok := first.config.Adapter.(domain.DirectMessageProvider)
	if !ok {
		slog.WarnContext(ctx, "Messaging adapter has no owner DM destination")
		return nil
	}
	ref, err := provider.DirectMessage(ctx, first.config.Owner)
	if err != nil {
		return err
	}
	msg := domain.OutboundMessage{Text: text}
	id, err := first.config.Adapter.Send(ctx, ref, msg)
	if err != nil {
		return err
	}
	if err := c.events.SaveOutbound(ctx, domain.SentMessage{ID: id, Conversation: ref, Message: msg, SentAt: time.Now()}); err != nil {
		return fmt.Errorf("record owner notification: %w", err)
	}
	return nil
}

func (c *core) acceptSession(info domain.SessionContext) bool {
	if info.Conversation == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.findRule(info.Conversation)
	return ok
}
func (c *core) runMessaging(ctx context.Context, instance *messagingInstance) error {
	for ctx.Err() == nil {
		err := instance.config.Adapter.Start(ctx, func(ctx context.Context, event domain.InboundEvent) error {
			for _, ref := range []domain.ConversationRef{event.Conversation, channel(event)} {
				owner, err := c.messaging.resolve(ref)
				if err != nil {
					return err
				}
				if owner != instance {
					return fmt.Errorf("event ref %q does not belong to messaging entry %q", ref, instance.config.Name)
				}
			}
			return c.receive(ctx, event)
		})
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, domain.ErrPersistence) {
			return err
		}
		slog.WarnContext(ctx, "Messaging connection ended; retrying", "messaging_entry", instance.config.Name, "error", err)
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}
