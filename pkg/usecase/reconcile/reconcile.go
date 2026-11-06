package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type Config struct {
	Workdir     string
	Actor       func(domain.SessionContext) domain.Actor
	IdleTimeout time.Duration
	TurnTimeout time.Duration
	TaskTimeout time.Duration
	RetryDelay  time.Duration
	MaxAttempts int
	Prompt      domain.PromptAssembler
	Prepare     func(context.Context, domain.InboxItem) (domain.UserInput, error)
	Procedure   func(context.Context, string) (string, error)
	NotifyOwner func(context.Context, string) error
	Completed   func(context.Context, string, time.Time) error
}

type Reconciler struct {
	config    Config
	inbox     domain.InboxRepository
	schedules domain.ScheduleRepository
	sessions  domain.SessionRepository
	events    domain.EventRepository
	model     domain.ModelAdapter
	hub       domain.ToolHub
	mu        sync.Mutex
	live      map[domain.SessionKey]*process
	changed   chan struct{}
	results   chan result
	workers   sync.WaitGroup
}

type process struct {
	key        domain.SessionKey
	token      string
	lane       string
	info       domain.SessionContext
	cancel     context.CancelFunc
	inputs     chan domain.InboxItem
	model      domain.ModelSession
	delivering *domain.InboxItem
	turn       *domain.SessionTurn
	closing    bool
	reason     string
	last       time.Time
}

type result struct {
	skipped   bool
	delivered bool
	exited    bool
	process   *process
	model     domain.ModelSession
	event     *domain.ModelEvent
	err       error
}

func New(config Config, inbox domain.InboxRepository, schedules domain.ScheduleRepository, sessions domain.SessionRepository, events domain.EventRepository, model domain.ModelAdapter, hub domain.ToolHub) *Reconciler {
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = time.Hour
	}
	if config.TurnTimeout <= 0 {
		config.TurnTimeout = 2 * time.Hour
	}
	if config.TaskTimeout <= 0 {
		config.TaskTimeout = 30 * time.Minute
	}
	if config.RetryDelay <= 0 {
		config.RetryDelay = time.Second
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 3
	}
	return &Reconciler{config: config, inbox: inbox, schedules: schedules, sessions: sessions, events: events, model: model, hub: hub, live: map[domain.SessionKey]*process{}, changed: make(chan struct{}, 1), results: make(chan result)}
}
func (r *Reconciler) Notify() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}
func (r *Reconciler) Context(key domain.SessionKey) (domain.SessionContext, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.live[key]
	if !ok || p.closing {
		return domain.SessionContext{}, false
	}
	return p.info, true
}

func (r *Reconciler) Start(ctx context.Context) (retErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		for _, p := range r.live {
			r.retire(p, "shutdown")
		}
		done := make(chan struct{})
		go func() { r.workers.Wait(); close(done) }()
		for {
			select {
			case <-done:
				return
			case out := <-r.results:
				retErr = errors.Join(retErr, r.handle(ctx, out))
			}
		}
	}()
	if err := r.sessions.Recover(ctx); err != nil {
		return err
	}
	for ctx.Err() == nil {
		delay, err := r.pass(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-r.changed:
			timer.Stop()
		case out := <-r.results:
			timer.Stop()
			if err := r.handle(ctx, out); err != nil {
				return err
			}
		case <-timer.C:
		}
	}
	return nil
}

func (r *Reconciler) pass(ctx context.Context) (time.Duration, error) {
	now := time.Now()
	delay := 24 * time.Hour
	due := func(at time.Time) { delay = min(delay, max(time.Duration(0), at.Sub(now))) }
	for _, p := range r.live {
		if p.closing {
			continue
		}
		if r.config.Actor != nil && p.info.TaskName == "" && (r.config.Actor(p.info) == "" || (p.info.Actor == domain.ActorOwner && r.config.Actor(p.info) != domain.ActorOwner)) {
			r.retire(p, "session permissions changed")
			continue
		}
		if p.turn != nil {
			if !now.Before(p.turn.Deadline) {
				r.retire(p, domain.TurnDeadlineExceeded)
				continue
			}
			due(p.turn.Deadline)
		}
		if p.delivering != nil {
			if !now.Before(p.delivering.Deadline) {
				r.retire(p, "delivery deadline exceeded")
				continue
			}
			due(p.delivering.Deadline)
		} else if p.turn == nil && p.model != nil {
			at := p.last.Add(r.config.IdleTimeout)
			if !now.Before(at) {
				if p.model.Idle() {
					r.retire(p, "idle")
				} else {
					due(now.Add(r.config.IdleTimeout))
				}
			} else {
				due(at)
			}
		}
	}
	schedules, err := r.schedules.List(ctx)
	if err != nil {
		return 0, err
	}
	for _, schedule := range schedules {
		if schedule.NextRun.After(now) {
			due(schedule.NextRun)
			continue
		}
		next, err := Next(schedule, now)
		if err != nil {
			return 0, err
		}
		var item *domain.InboxItem
		prompt := schedule.Prompt
		if r.config.Procedure != nil && (schedule.Name == "heartbeat" || schedule.Name == "consolidate") {
			// File reads run with the item, never in the reconciliation loop.
			prompt = "Run the " + schedule.Name + " procedure."
		}
		if prompt != "" {
			value := TaskItem(schedule.Name, prompt, schedule.Workdir)
			if r.config.Procedure != nil && (schedule.Name == "heartbeat" || schedule.Name == "consolidate") {
				value.Payload.Procedure = schedule.Name
			}
			value.Actor = schedule.Actor
			value.Payload.Context.Actor = schedule.Actor
			value.SourceKey = fmt.Sprintf("schedule:%s:%s", schedule.Name, schedule.NextRun.Format(time.RFC3339Nano))
			item = &value
		}
		if err := r.inbox.Fire(ctx, schedule, next, item); err != nil {
			return 0, err
		}
		due(next)
	}
	items, err := r.inbox.Active(ctx)
	if err != nil {
		return 0, err
	}
	taskBusy := false
	for _, p := range r.live {
		if p.lane == "task" && (p.delivering != nil || p.turn != nil || p.closing) {
			taskBusy = true
		}
	}
	blocked := map[domain.SessionKey]bool{}
	for _, item := range items {
		if blocked[item.SessionKey] {
			continue
		}
		blocked[item.SessionKey] = true
		p := r.live[item.SessionKey]
		if item.Status != "pending" || (p != nil && (p.delivering != nil || p.closing)) || (item.Lane == "task" && taskBusy) {
			continue
		}
		at := item.ReadyAt
		if item.RetryAt.After(at) {
			at = item.RetryAt
		}
		if at.After(now) {
			due(at)
			continue
		}
		if r.config.Actor != nil {
			item.Actor = r.config.Actor(item.Payload.Context)
		}
		if p != nil && p.info.Actor == domain.ActorMember {
			item.Actor = domain.ActorMember
		}
		item.Payload.Context.Actor = item.Actor
		if item.Actor == "" {
			if err := r.inbox.Reject(ctx, item.ID, "session no longer has an accept rule"); err != nil {
				return 0, err
			}
			r.Notify()
			continue
		}
		if p != nil && (p.info.Actor != item.Actor || p.info.Workdir != item.Payload.Context.Workdir) {
			r.retire(p, "session configuration changed")
			continue
		}
		item.Deadline = now.Add(r.timeout(item.Lane))
		claimed, err := r.inbox.Claim(ctx, item.ID, item.Deadline)
		if err != nil {
			return 0, err
		}
		if !claimed {
			continue
		}
		if p == nil {
			saved, err := r.sessions.Get(ctx, item.SessionKey)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return 0, err
			}
			endpoint, err := r.hub.OpenSession(item.SessionKey, item.Actor)
			if err != nil {
				if err := r.fail(ctx, item, err); err != nil {
					return 0, err
				}
				continue
			}
			processCtx, cancel := context.WithCancel(ctx)
			p = &process{key: item.SessionKey, token: saved.ResumeToken, lane: item.Lane, info: item.Payload.Context, cancel: cancel, inputs: make(chan domain.InboxItem, 1), last: now}
			p.info.Actor = item.Actor
			r.mu.Lock()
			r.live[item.SessionKey] = p
			r.mu.Unlock()
			r.workers.Add(1)
			go r.pump(processCtx, p, saved.ResumeToken, endpoint)
		}
		p.delivering = &item
		p.last = now
		p.inputs <- item
		if item.Lane == "task" {
			taskBusy = true
		}
		due(item.Deadline)
	}
	return delay, nil
}

func (r *Reconciler) timeout(lane string) time.Duration {
	if lane == "task" {
		return r.config.TaskTimeout
	}
	return r.config.TurnTimeout
}

func (r *Reconciler) pump(ctx context.Context, p *process, token string, endpoint domain.MCPEndpoint) {
	defer r.workers.Done()
	emit := func(out result) {
		out.process = p
		r.results <- out
	}
	var model domain.ModelSession
	var events <-chan domain.ModelEvent
	defer func() {
		if model != nil {
			if err := model.Close(); err != nil {
				slog.WarnContext(ctx, "Model close failed", "session", p.key, "error", err)
			}
		}
		emit(result{exited: true})
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-p.inputs:
			sendCtx, cancel := context.WithDeadline(ctx, item.Deadline)
			if item.Payload.Notice != "" {
				var err error
				if r.config.NotifyOwner == nil {
					err = errors.New("owner notification destination unavailable")
				} else {
					err = r.config.NotifyOwner(sendCtx, item.Payload.Notice)
				}
				cancel()
				emit(result{delivered: true, skipped: true, err: err})
				return
			}
			input := item.Payload.Input
			var err error
			if r.config.Procedure != nil && item.Payload.Procedure != "" {
				input.Text, err = r.config.Procedure(sendCtx, item.Payload.Procedure)
				if err == nil && input.Text == "" {
					cancel()
					emit(result{delivered: true, skipped: true})
					continue
				}
			}
			if err == nil && r.config.Prepare != nil {
				item.Payload.Input = input
				input, err = r.config.Prepare(sendCtx, item)
			}
			if err == nil && model == nil {
				prompt := "Use the reply tool to post messages. Final assistant text is private."
				if r.config.Prompt != nil {
					var extra string
					extra, err = r.config.Prompt(sendCtx, domain.ToolSession{Key: p.key, Actor: p.info.Actor})
					prompt += "\n\n" + extra
				}
				if err == nil {
					model, err = r.model.Open(ctx, domain.ModelSessionConfig{Key: p.key, ResumeToken: token, ToolHub: endpoint, Workdir: p.info.Workdir, SystemPrompt: prompt})
				}
				if err == nil {
					events = model.Events()
					emit(result{model: model})
				}
			}
			if err == nil {
				err = model.Send(sendCtx, input)
			}
			cancel()
			emit(result{delivered: true, err: err})
			if err != nil {
				if errors.Is(err, domain.ErrRejected) {
					continue
				}
				return
			}
		case event, ok := <-events:
			if !ok {
				emit(result{err: errors.New("model process exited")})
				return
			}
			emit(result{event: &event})
		}
	}
}

func (r *Reconciler) handle(ctx context.Context, out result) error {
	// Shutdown must record successful delivery before recovery can retry the row.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	p := out.process
	if r.live[p.key] != p {
		return nil
	}
	if out.exited {
		reason := p.reason
		if reason == "" {
			reason = "model process exited"
		}
		if err := r.sessions.Interrupt(ctx, p.key, reason); err != nil {
			return err
		}
		r.retire(p, reason)
		r.mu.Lock()
		delete(r.live, p.key)
		r.mu.Unlock()
		return nil
	}
	if out.model != nil {
		p.model = out.model
		return r.save(ctx, p)
	}
	if out.delivered {
		item := p.delivering
		p.delivering = nil
		if out.err != nil {
			if p.closing || errors.Is(out.err, context.DeadlineExceeded) {
				r.retire(p, "delivery deadline exceeded")
				return nil
			}
			if err := r.fail(ctx, *item, out.err); err != nil {
				return err
			}
			if !errors.Is(out.err, domain.ErrRejected) || p.model == nil {
				r.retire(p, out.err.Error())
			}
			return nil
		}
		if !out.skipped && p.turn == nil {
			p.turn = &domain.SessionTurn{Deadline: time.Now().Add(r.timeout(p.lane)), Procedure: item.Payload.Procedure, Continuations: item.Payload.Continuations}
		}
		if err := r.inbox.Delivered(ctx, item.ID, r.state(p)); err != nil {
			return err
		}
		p.last = time.Now()
		if p.model == nil {
			r.retire(p, "empty input")
		}
		return nil
	}
	if p.closing {
		return nil
	}
	if out.err != nil {
		r.retire(p, out.err.Error())
		return nil
	}
	event := *out.event
	p.last = time.Now()
	if err := r.events.SaveModel(ctx, domain.ModelRecord{SessionKey: p.key, Event: event, RecordedAt: p.last}); err != nil {
		return err
	}
	switch event.Kind {
	case domain.ModelEventTurnStarted:
		if p.turn == nil {
			p.turn = &domain.SessionTurn{Deadline: p.last.Add(r.timeout(p.lane))}
		}
	case domain.ModelEventError:
		if p.turn != nil {
			p.turn.Failed = true
		}
	case domain.ModelEventTurnFinished:
		if p.turn != nil && !p.turn.Failed && r.config.Completed != nil {
			if err := r.config.Completed(ctx, p.turn.Procedure, p.last); err != nil {
				return err
			}
		}
		p.turn = nil
	}
	return r.save(ctx, p)
}
func (r *Reconciler) state(p *process) domain.Session {
	s := domain.Session{Key: p.key, ResumeToken: p.token, Context: &p.info, Turn: p.turn}
	if p.model != nil {
		if token := p.model.ResumeToken(); token != "" {
			p.token = token
			s.ResumeToken = token
		}
	}
	return s
}
func (r *Reconciler) save(ctx context.Context, p *process) error {
	return r.sessions.Save(ctx, r.state(p))
}
func (r *Reconciler) retire(p *process, reason string) {
	if p.closing {
		return
	}
	r.mu.Lock()
	p.closing = true
	r.mu.Unlock()
	p.reason = reason
	r.hub.CloseSession(p.key)
	p.cancel()
}
func (r *Reconciler) fail(ctx context.Context, item domain.InboxItem, cause error) error {
	if errors.Is(cause, domain.ErrRejected) {
		r.Notify()
		return r.inbox.Reject(ctx, item.ID, cause.Error())
	}
	retry := time.Now().Add(r.config.RetryDelay * time.Duration(1<<min(item.Attempts, 8)))
	dead, err := r.inbox.Finish(ctx, item.ID, cause.Error(), r.config.MaxAttempts, retry)
	if err != nil {
		return err
	}
	r.Notify()
	slog.WarnContext(ctx, "Inbox delivery failed", "item", item.ID, "session", item.SessionKey, "error", cause, "dead", dead)
	return nil
}
