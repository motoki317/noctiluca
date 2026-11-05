package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/motoki317/noctiluca/pkg/domain"
	memoryusecase "github.com/motoki317/noctiluca/pkg/usecase/memory"
	"github.com/motoki317/noctiluca/pkg/usecase/reconcile"
	"golang.org/x/sync/errgroup"
)

type Config struct {
	Owner                string
	Rules                map[domain.ConversationRef]domain.AcceptRule
	DirectMessages       *domain.AcceptRule
	UserGroups           map[string][]string
	IdleTimeout          time.Duration
	TaskTimeout          time.Duration
	TurnTimeout          time.Duration
	RetryDelay           time.Duration
	MaxAttempts          int
	GroupRefreshInterval time.Duration
	Workdir              string
	Jobs                 []domain.Schedule
	Procedure            func(context.Context, string) (string, error)
	Completed            func(context.Context, string, time.Time) error
}
type rule struct {
	users                     map[string]struct{}
	allowBots, requireMention bool
	actor                     domain.Actor
}
type core struct {
	config           Config
	rules            compiledRules
	messaging        domain.MessagingAdapter
	events           domain.EventRepository
	sessions         domain.SessionRepository
	inbox            domain.InboxRepository
	prompt           domain.PromptAssembler
	hub              domain.ToolHub
	reconciler       *reconcile.Reconciler
	external         domain.ExternalTools
	mu               sync.Mutex
	cancel           context.CancelFunc
	started, stopped bool
	done             chan struct{}
	groupIDs         []string
}

func New(config Config, messaging domain.MessagingAdapter, model domain.ModelAdapter, hub domain.ToolHub, events domain.EventRepository, sessions domain.SessionRepository, prompt domain.PromptAssembler, schedules domain.ScheduleRepository, external domain.ExternalTools, memory domain.MemoryStore, inbox domain.InboxRepository) (domain.Core, error) {
	if messaging == nil || model == nil || hub == nil || events == nil || sessions == nil || schedules == nil || inbox == nil {
		return nil, errors.New("core dependencies are required")
	}
	if config.IdleTimeout < 0 || config.TaskTimeout < 0 || config.GroupRefreshInterval < 0 {
		return nil, errors.New("timeouts must be positive")
	}
	if config.GroupRefreshInterval == 0 {
		config.GroupRefreshInterval = time.Hour
	}
	config.Rules = cloneRules(config.Rules)
	config.UserGroups = cloneGroups(config.UserGroups)
	if config.DirectMessages != nil {
		direct := cloneRule(*config.DirectMessages)
		config.DirectMessages = &direct
	}
	all := make([]domain.AcceptRule, 0, len(config.Rules)+1)
	for _, r := range config.Rules {
		all = append(all, r)
	}
	if config.DirectMessages != nil {
		all = append(all, *config.DirectMessages)
	}
	var ids []string
	for _, r := range all {
		for _, id := range r.Groups {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	if _, ok := messaging.(domain.UserGroupProvider); ok {
		for _, id := range ids {
			if _, exists := config.UserGroups[id]; !exists {
				config.UserGroups[id] = nil
			}
		}
	}
	rules, err := compileRules(config)
	if err != nil {
		return nil, err
	}
	c := &core{config: config, rules: rules, messaging: messaging, events: events, sessions: sessions, inbox: inbox, hub: hub, external: external, done: make(chan struct{}), groupIDs: ids, prompt: memoryusecase.Prompt(memory, messaging.ToolNotes(), prompt)}
	if filter, ok := messaging.(domain.ConversationFilter); ok {
		filter.SetConversationFilter(func(ref domain.ConversationRef) bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			rule, ok := c.findRule(c.rules, ref, "")
			return ok && len(rule.users) > 0
		})
	}
	c.reconciler = reconcile.New(reconcile.Config{Workdir: config.Workdir, IdleTimeout: config.IdleTimeout, TurnTimeout: config.TurnTimeout, TaskTimeout: config.TaskTimeout, RetryDelay: config.RetryDelay, MaxAttempts: config.MaxAttempts, Prepare: c.prepare, Actor: c.actor, Procedure: config.Procedure, NotifyOwner: c.notifyOwner, Completed: config.Completed}, inbox, schedules, sessions, events, model, hub)
	tools := []domain.Tool{c.replyTool()}
	if err := hub.AddSource("core", tools); err != nil {
		return nil, err
	}
	if err := hub.AddSource("messaging", messaging.Tools()); err != nil {
		return nil, err
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
		result = errors.Join(result, c.hub.Shutdown(stop), c.messaging.Shutdown(stop))
		if c.external != nil {
			result = errors.Join(result, c.external.Shutdown(stop))
		}
		if ctx.Err() != nil && errors.Is(result, ctx.Err()) {
			result = nil
		}
	}()
	if c.external != nil {
		if err := c.external.Connect(ctx); err != nil {
			slog.WarnContext(ctx, "External tools unavailable", "error", err)
		}
	}
	if provider, ok := c.messaging.(domain.UserGroupProvider); ok && len(c.groupIDs) > 0 {
		groups, err := provider.UserGroups(ctx, c.groupIDs)
		if err != nil {
			slog.WarnContext(ctx, "User group lookup failed", "error", err)
			groups = c.config.UserGroups
		}
		if err := c.applyGroups(groups); err != nil {
			return err
		}
	}
	g, runCtx := errgroup.WithContext(ctx)
	g.Go(func() error { return c.hub.Start(runCtx) })
	g.Go(func() error { return c.reconciler.Start(runCtx) })
	g.Go(func() error { return c.runMessaging(runCtx) })
	if c.external != nil {
		g.Go(func() error { return c.external.Start(runCtx) })
	}
	if provider, ok := c.messaging.(domain.UserGroupProvider); ok && len(c.groupIDs) > 0 {
		g.Go(func() error { return c.refreshGroups(runCtx, provider) })
	}
	result = g.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return result
}
func (c *core) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	if !c.started {
		c.stopped = true
		c.mu.Unlock()
		return c.hub.Shutdown(ctx)
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
	r, ok := c.findRule(c.rules, event.Conversation, event.Parent)
	if !ok || event.Conversation == "" || (r.requireMention && !event.MentionsMe) {
		return r, false
	}
	if event.Sender.Kind != domain.SenderHuman && (!r.allowBots || event.Sender.Kind != domain.SenderBot) {
		return r, false
	}
	_, ok = r.users[event.Sender.ID]
	return r, ok
}
func (c *core) receive(ctx context.Context, event domain.InboundEvent) error {
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = time.Now()
	}
	if event.SourceKey == "" {
		event.SourceKey = uuid.NewString()
	}
	c.mu.Lock()
	r, accepted := c.accepted(event)
	c.mu.Unlock()
	event.Accepted = &accepted
	actor := r.actor
	if actor == "" {
		actor = domain.ActorMember
	}
	item := domain.InboxItem{SourceKey: event.SourceKey, SessionKey: domain.SessionKey(event.Conversation), Actor: actor, Lane: "conv", ReadyAt: event.ReceivedAt, Payload: domain.InboxPayload{Event: &event, Context: domain.SessionContext{Conversation: event.Conversation, Parent: event.Parent, Actor: actor, Workdir: c.config.Workdir}}}
	if !accepted {
		item.Status = "done"
	}
	if err := c.inbox.Receive(ctx, event, item); err != nil {
		return errors.Join(domain.ErrPersistence, err)
	}
	c.reconciler.Notify()
	return nil
}
func (c *core) prepare(ctx context.Context, item domain.InboxItem, first bool) (domain.UserInput, error) {
	input := item.Payload.Input
	if event := item.Payload.Event; event != nil {
		c.mu.Lock()
		_, accepted := c.accepted(*event)
		c.mu.Unlock()
		if !accepted {
			return input, fmt.Errorf("%w: conversation no longer accepts this sender", domain.ErrRejected)
		}
		var history []domain.InboundEvent
		if first && event.Parent != "" {
			var err error
			history, err = c.events.ListInbound(ctx, event.Parent, 30)
			if err != nil {
				return input, err
			}
			slices.Reverse(history)
		}
		raw, err := json.Marshal(struct {
			ParentHistory []domain.InboundEvent `json:"parent_history,omitempty"`
			Event         domain.InboundEvent   `json:"event"`
		}{history, *event})
		if err != nil {
			return input, err
		}
		input = domain.UserInput{Text: string(raw), Attachments: event.Attachments}
	}
	return c.prompt(ctx, domain.ToolSession{Key: item.SessionKey, Actor: item.Actor}, input)
}
func (c *core) workItem(ctx context.Context, key domain.SessionKey) (domain.InboxItem, error) {
	info, ok := c.reconciler.Context(key)
	if !ok {
		saved, err := c.sessions.Get(ctx, key)
		if err != nil {
			return domain.InboxItem{}, err
		}
		if saved.Context == nil {
			return domain.InboxItem{}, errors.New("session context unavailable")
		}
		info = *saved.Context
	}
	lane := "conv"
	if info.TaskName != "" {
		lane = "task"
	}
	return domain.InboxItem{SessionKey: key, Actor: info.Actor, Lane: lane, Payload: domain.InboxPayload{Context: info}}, nil
}

func (c *core) notifyOwner(ctx context.Context, text string) error {
	provider, ok := c.messaging.(domain.DirectMessageProvider)
	if !ok {
		slog.WarnContext(ctx, "Messaging adapter has no owner DM destination")
		return nil
	}
	ref, err := provider.DirectMessage(ctx, c.config.Owner)
	if err != nil {
		return err
	}
	msg := domain.OutboundMessage{Text: text}
	id, err := c.messaging.Send(ctx, ref, msg)
	if err != nil {
		return err
	}
	if err := c.events.SaveOutbound(ctx, domain.SentMessage{ID: id, Conversation: ref, Message: msg, SentAt: time.Now()}); err != nil {
		return fmt.Errorf("record owner notification: %w", err)
	}
	return nil
}

func (c *core) actor(info domain.SessionContext) domain.Actor {
	if info.TaskName != "" {
		return domain.ActorOwner
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.findRule(c.rules, info.Conversation, info.Parent)
	if !ok || len(r.users) == 0 {
		return ""
	}
	return r.actor
}
func (c *core) runMessaging(ctx context.Context) error {
	for ctx.Err() == nil {
		err := c.messaging.Start(ctx, c.receive)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, domain.ErrPersistence) {
			return err
		}
		slog.WarnContext(ctx, "Messaging connection ended; retrying", "error", err)
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
