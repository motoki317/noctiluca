package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type Config struct {
	Workdir     string
	Accept      func(domain.SessionContext) bool
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
	watches   domain.WatchRepository
	schedules domain.ScheduleRepository
	sessions  domain.SessionRepository
	events    domain.EventRepository
	model     domain.ModelAdapter
	hub       domain.ToolHub
	mu        sync.Mutex
	live      map[domain.SessionKey]*process
	taskFires map[domain.SessionKey]time.Time
	changed   chan struct{}
	results   chan result
	workers   sync.WaitGroup
}

type process struct {
	key          domain.SessionKey
	token        string
	lane         string
	info         domain.SessionContext
	cancel       context.CancelFunc
	inputs       chan domain.InboxItem
	model        domain.ModelSession
	delivering   *domain.InboxItem
	unstarted    map[int64]domain.InboxItem
	turn         *domain.SessionTurn
	restArmed    bool
	restDeadline time.Time
	closing      bool
	reason       string
	retiredAt    time.Time
	last         time.Time
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

func New(config Config, inbox domain.InboxRepository, watches domain.WatchRepository, schedules domain.ScheduleRepository, sessions domain.SessionRepository, events domain.EventRepository, model domain.ModelAdapter, hub domain.ToolHub) *Reconciler {
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
	return &Reconciler{config: config, inbox: inbox, watches: watches, schedules: schedules, sessions: sessions, events: events, model: model, hub: hub, live: map[domain.SessionKey]*process{}, taskFires: map[domain.SessionKey]time.Time{}, changed: make(chan struct{}, 1), results: make(chan result)}
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
		if r.config.Accept != nil && !r.config.Accept(p.info) {
			r.retire(p, domain.ConversationNoLongerAccepted)
			continue
		}
		if !p.restDeadline.IsZero() {
			if p.idle() || !now.Before(p.restDeadline) {
				r.retire(p, "rest")
			} else {
				due(p.restDeadline)
			}
			continue
		}
		for _, item := range p.unstarted {
			if !now.Before(item.Deadline) {
				r.retire(p, domain.DeliveryDeadlineExceeded)
				break
			}
			due(item.Deadline)
		}
		if p.closing {
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
				r.retire(p, domain.DeliveryDeadlineExceeded)
				continue
			}
			due(p.delivering.Deadline)
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
			value.SourceKey = fmt.Sprintf("schedule:%s:%s", schedule.Name, schedule.NextRun.Format(time.RFC3339Nano))
			item = &value
		}
		if item != nil && item.Payload.Procedure == "consolidate" {
			sessions, err := r.sessions.List(ctx)
			if err != nil {
				return 0, err
			}
			for _, session := range sessions {
				if session.ResumeToken == "" || session.Context == nil || session.Context.TaskName != "" || (r.config.Accept != nil && !r.config.Accept(*session.Context)) {
					continue
				}
				if _, err := r.inbox.Enqueue(ctx, domain.InboxItem{
					SourceKey:  fmt.Sprintf("rest:%s:%s", session.Key, schedule.NextRun.Format(time.RFC3339Nano)),
					SessionKey: session.Key,
					Lane:       "conv",
					ReadyAt:    schedule.NextRun,
					Payload:    domain.InboxPayload{Context: *session.Context, Procedure: "rest"},
				}); err != nil {
					return 0, err
				}
			}
		}
		if err := r.inbox.Fire(ctx, schedule, next, item); err != nil {
			return 0, err
		}
		due(next)
	}
	taskRests := map[domain.SessionKey]time.Time{}
	for _, p := range r.live {
		if !p.restArmed || !p.idle() {
			continue
		}
		last, next, err := lastTaskFire(schedules, p.info.TaskName, r.taskFires[p.key])
		if err != nil {
			return 0, err
		}
		if last {
			taskRests[p.key] = next
		}
	}
	watching := map[domain.SessionKey]bool{}
	if len(taskRests) != 0 {
		watches, err := r.watches.Unfinished(ctx)
		if err != nil {
			return 0, err
		}
		for _, watch := range watches {
			watching[watch.Item.SessionKey] = true
		}
	}
	// Watch completion atomically enqueues its result, so read watches before inbox items.
	items, err := r.inbox.Active(ctx)
	if err != nil {
		return 0, err
	}
	readyAt := func(item domain.InboxItem) time.Time {
		if item.RetryAt.After(item.ReadyAt) {
			return item.RetryAt
		}
		return item.ReadyAt
	}
	queuedWork := map[domain.SessionKey]bool{}
	queuedRest := map[domain.SessionKey]string{}
	for _, item := range items {
		if item.Payload.Procedure == "rest" {
			queuedRest[item.SessionKey] = item.Lane
			continue
		}
		at := readyAt(item)
		holdsRest := !at.After(now)
		if next := taskRests[item.SessionKey]; !next.IsZero() {
			holdsRest = at.Before(next)
		}
		if item.Status != "pending" || holdsRest {
			queuedWork[item.SessionKey] = true
		}
	}
	for key := range taskRests {
		p := r.live[key]
		if queuedRest[key] != "" || queuedWork[key] || watching[key] {
			continue
		}
		item := domain.InboxItem{
			SourceKey:  fmt.Sprintf("rest:%s:%s", p.key, now.Format(time.RFC3339Nano)),
			SessionKey: p.key, Lane: "task", ReadyAt: now, Status: "pending",
			Payload: domain.InboxPayload{Context: p.info, Procedure: "rest"},
		}
		item.ID, err = r.inbox.Enqueue(ctx, item)
		if err != nil {
			return 0, err
		}
		p.restArmed = false
		queuedRest[key] = "task"
		items = append(items, item)
	}
	// Conversation rests keep their order ahead of consolidation.
	sort.SliceStable(items, func(i, j int) bool {
		iRest := items[i].Lane == "task" && items[i].Payload.Procedure == "rest"
		jRest := items[j].Lane == "task" && items[j].Payload.Procedure == "rest"
		return iRest && !jRest
	})
	for _, p := range r.live {
		if p.closing || p.resting() || queuedRest[p.key] == "task" || p.delivering != nil || p.turn != nil || len(p.unstarted) != 0 || p.model == nil {
			continue
		}
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
	taskBusy := false
	for _, p := range r.live {
		if p.resting() || (p.lane == "task" && (p.delivering != nil || len(p.unstarted) != 0 || p.turn != nil || p.closing)) {
			taskBusy = true
		}
	}
	blocked := map[domain.SessionKey]bool{}
	for _, item := range items {
		p := r.live[item.SessionKey]
		if p != nil {
			if _, sent := p.unstarted[item.ID]; sent {
				continue
			}
		}
		rest := item.Payload.Procedure == "rest"
		if !rest && (blocked[item.SessionKey] || queuedRest[item.SessionKey] == "task") {
			continue
		}
		if rest && ((item.Lane != "task" && queuedWork[item.SessionKey]) || taskBusy || (p != nil && !p.idle())) {
			continue
		}
		if !rest {
			blocked[item.SessionKey] = true
		}
		if item.Status != "pending" || (p != nil && (p.delivering != nil || p.closing || p.resting())) || (item.Lane == "task" && taskBusy) {
			continue
		}
		at := readyAt(item)
		if at.After(now) {
			due(at)
			continue
		}
		if r.config.Accept != nil && !r.config.Accept(item.Payload.Context) {
			if err := r.inbox.Reject(ctx, item.ID, "session no longer has an accept rule"); err != nil {
				return 0, err
			}
			r.Notify()
			continue
		}
		if p != nil && p.info.Workdir != item.Payload.Context.Workdir {
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
		blocked[item.SessionKey] = true
		var saved domain.Session
		if p == nil || rest {
			saved, err = r.sessions.Get(ctx, item.SessionKey)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return 0, err
			}
			if errors.Is(err, domain.ErrNotFound) {
				saved = domain.Session{Key: item.SessionKey, Context: &item.Payload.Context}
			}
		}
		if rest && saved.ResumeToken == "" {
			if err := r.inbox.Delivered(ctx, item.ID, saved); err != nil {
				return 0, err
			}
			r.Notify()
			continue
		}
		if p == nil {
			endpoint, err := r.hub.OpenSession(item.SessionKey)
			if err != nil {
				if err := r.fail(ctx, item, err); err != nil {
					return 0, err
				}
				continue
			}
			processCtx, cancel := context.WithCancel(ctx)
			p = &process{key: item.SessionKey, token: saved.ResumeToken, lane: item.Lane, info: item.Payload.Context, cancel: cancel, inputs: make(chan domain.InboxItem, 1), last: now}
			r.mu.Lock()
			r.live[item.SessionKey] = p
			r.mu.Unlock()
			r.workers.Add(1)
			go r.pump(processCtx, p, saved.ResumeToken, endpoint)
		}
		if item.Lane == "task" && !rest {
			fire, err := taskFire(item)
			if err != nil {
				return 0, err
			}
			if fire.IsZero() {
				fire = r.taskFires[item.SessionKey]
				for _, candidate := range items {
					if candidate.SessionKey != item.SessionKey {
						continue
					}
					at, err := taskFire(candidate)
					if err != nil {
						return 0, err
					}
					if at.After(fire) && !at.After(readyAt(item)) {
						fire = at
					}
				}
			}
			r.taskFires[item.SessionKey] = fire
		}
		p.delivering = &item
		p.last = now
		p.inputs <- item
		if item.Lane == "task" || rest {
			taskBusy = true
		}
		due(item.Deadline)
	}
	return delay, nil
}

func (p *process) idle() bool {
	return !p.closing && p.delivering == nil && len(p.unstarted) == 0 && p.turn == nil && p.model != nil && p.model.Idle()
}

func (p *process) resting() bool {
	if !p.restDeadline.IsZero() || (p.turn != nil && p.turn.Procedure == "rest") || (p.delivering != nil && p.delivering.Payload.Procedure == "rest") {
		return true
	}
	for _, item := range p.unstarted {
		if item.Payload.Procedure == "rest" {
			return true
		}
	}
	return false
}

func endsSession(procedure string) bool {
	return procedure == "rest" || procedure == "consolidate"
}

func taskFire(item domain.InboxItem) (time.Time, error) {
	stamp, ok := strings.CutPrefix(item.SourceKey, "schedule:"+item.Payload.Context.TaskName+":")
	if !ok {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, stamp)
}

func lastTaskFire(schedules []domain.Schedule, name string, fire time.Time) (bool, time.Time, error) {
	if fire.IsZero() {
		return false, time.Time{}, nil
	}
	var boundary, next time.Time
	for _, schedule := range schedules {
		if schedule.Name != "consolidate" && schedule.Name != name {
			continue
		}
		at, err := Next(schedule, fire)
		if err != nil {
			return false, time.Time{}, err
		}
		if schedule.Name == "consolidate" {
			boundary = at
		} else {
			next = at
		}
	}
	return !boundary.IsZero() && (next.IsZero() || !boundary.After(next)), next, nil
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
		for events != nil {
			select {
			case event, ok := <-events:
				if !ok {
					events = nil
					continue
				}
				emit(result{event: &event})
			default:
				events = nil
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
					extra, err = r.config.Prompt(sendCtx, domain.ToolSession{Key: p.key})
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
				input.ID = item.ID
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

// handleWriteTimeout bounds the writes of one handle call. Writes slower than this mean storage is stuck:
// stop the process and let restart recovery reconcile from SQLite.
const handleWriteTimeout = 30 * time.Second

func (r *Reconciler) handle(ctx context.Context, out result) error {
	// Shutdown must record successful delivery before recovery can retry the row.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handleWriteTimeout)
	defer cancel()
	p := out.process
	if r.live[p.key] != p {
		return nil
	}
	if out.exited {
		// The pump forwards buffered events before this exit, so a row that started before
		// the close is already done and no longer unstarted.
		if p.reason == domain.DeliveryDeadlineExceeded {
			for _, item := range p.unstarted {
				if !p.retiredAt.Before(item.Deadline) {
					if err := r.fail(ctx, item, context.DeadlineExceeded); err != nil {
						return err
					}
				}
			}
		}
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
			if p.closing && (p.reason != domain.DeliveryDeadlineExceeded || p.retiredAt.Before(item.Deadline)) {
				return nil
			}
			cause := out.err
			if p.reason == domain.DeliveryDeadlineExceeded {
				// Retirement cancels delivery, which can hide the deadline in its error.
				cause = context.DeadlineExceeded
			}
			if err := r.fail(ctx, *item, cause); err != nil {
				return err
			}
			if errors.Is(cause, context.DeadlineExceeded) {
				r.retire(p, domain.DeliveryDeadlineExceeded)
			} else if !errors.Is(cause, domain.ErrRejected) || p.model == nil {
				r.retire(p, cause.Error())
			}
			return nil
		}
		if p.lane == "task" && !out.skipped && !time.Now().Before(item.Deadline) {
			r.retire(p, domain.DeliveryDeadlineExceeded)
		}
		if !out.skipped && p.turn == nil {
			p.turn = &domain.SessionTurn{InputID: item.ID, Deadline: time.Now().Add(r.timeout(p.lane)), Procedure: item.Payload.Procedure, Continuations: item.Payload.Continuations}
		}
		if !out.skipped {
			if p.unstarted == nil {
				p.unstarted = make(map[int64]domain.InboxItem)
			}
			p.unstarted[item.ID] = *item
			return r.save(ctx, p)
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
	if out.event != nil && (out.event.Kind == domain.ModelEventInputStarted || out.event.Kind == domain.ModelEventInputRefused) {
		item, found := p.unstarted[out.event.InputID]
		if !found {
			return nil
		}
		delete(p.unstarted, item.ID)
		if out.event.Kind == domain.ModelEventInputRefused {
			if endsSession(item.Payload.Procedure) {
				p.restDeadline = time.Time{}
			}
			if err := r.fail(ctx, item, out.event.Err); err != nil {
				return err
			}
			if p.turn != nil {
				if p.turn.InputID == item.ID && len(p.unstarted) == 0 {
					p.turn = nil
				} else {
					p.turn.Failed = true
				}
			}
			return r.save(ctx, p)
		}
		p.last = time.Now()
		return r.inbox.Delivered(ctx, item.ID, r.state(p))
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
		turn := p.turn
		if turn != nil {
			if !turn.Failed && endsSession(turn.Procedure) {
				p.restDeadline = turn.Deadline
			} else if !endsSession(turn.Procedure) && p.lane == "task" && r.config.Procedure != nil && !r.taskFires[p.key].IsZero() {
				p.restArmed = true
			}
		}
		p.turn = nil
		if err := r.save(ctx, p); err != nil {
			return err
		}
		if turn != nil && !turn.Failed && r.config.Completed != nil {
			return r.config.Completed(ctx, turn.Procedure, p.last)
		}
		return nil
	}
	return r.save(ctx, p)
}
func (r *Reconciler) state(p *process) domain.Session {
	s := domain.Session{Key: p.key, ResumeToken: p.token, Context: &p.info, Turn: p.turn}
	if !p.restDeadline.IsZero() {
		s.ResumeToken = ""
		s.Turn = nil
	} else if p.model != nil {
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
	p.retiredAt = time.Now()
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
