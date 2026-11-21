package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/motoki317/noctiluca/pkg/domain"
)

const (
	operationTimeout   = 30 * time.Minute
	modelProcessExited = "model process exited"
	compactNudgeTokens = 200000
)

type Config struct {
	Workdir          string
	ChatWritingTools []string
	Accept           func(domain.SessionContext) bool
	RetryDelay       time.Duration
	MaxAttempts      int
	Prompt           domain.PromptAssembler
	Prepare          func(context.Context, domain.InboxItem) (domain.UserInput, error)
	Procedure        func(context.Context, string) (string, error)
	NotifyOwner      func(context.Context, string) error
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
	key         domain.SessionKey
	token       string
	cancel      context.CancelFunc
	inputs      chan domain.InboxItem
	model       domain.ModelSession
	delivering  *domain.InboxItem
	unstarted   map[int64]domain.InboxItem
	turn        *domain.SessionTurn
	turnStarted bool
	closeAtIdle bool
	closing     bool
	reason      string
	lastError   string
}

type result struct {
	action     sessionAction
	actionDone chan error
	skipped    bool
	delivered  bool
	exited     bool
	process    *process
	model      domain.ModelSession
	event      *domain.ModelEvent
	err        error
}

func New(config Config, inbox domain.InboxRepository, schedules domain.ScheduleRepository, sessions domain.SessionRepository, events domain.EventRepository, model domain.ModelAdapter, hub domain.ToolHub) *Reconciler {
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
		if r.config.Procedure != nil && (schedule.Name == "heartbeat" || schedule.Name == "rest") {
			prompt = "Run the " + schedule.Name + " procedure."
		}
		if prompt != "" {
			value := TaskItem(schedule.Name, prompt, schedule.Workdir)
			if r.config.Procedure != nil && (schedule.Name == "heartbeat" || schedule.Name == "rest") {
				value.Payload.Procedure = schedule.Name
			}
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
	readyAt := func(item domain.InboxItem) time.Time {
		if item.RetryAt.After(item.ReadyAt) {
			return item.RetryAt
		}
		return item.ReadyAt
	}
	blocked := map[domain.SessionKey]bool{}
	for _, item := range items {
		// A database from a version with per-conversation sessions holds rows with other keys. They run in main.
		if !item.SessionKey.IsNotice() {
			item.SessionKey = domain.MainSessionKey
		}
		p := r.live[item.SessionKey]
		if p != nil {
			if _, sent := p.unstarted[item.ID]; sent {
				continue
			}
		}
		if blocked[item.SessionKey] {
			continue
		}
		if item.Status != "pending" || (p != nil && (p.delivering != nil || p.closing)) {
			continue
		}
		at := readyAt(item)
		if at.After(now) {
			due(at)
			continue
		}
		blocked[item.SessionKey] = true
		if r.config.Accept != nil && !r.config.Accept(item.Payload.Context) {
			if err := r.inbox.Reject(ctx, item.ID, "session no longer has an accept rule"); err != nil {
				return 0, err
			}
			r.Notify()
			continue
		}
		claimed, err := r.inbox.Claim(ctx, item.ID)
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
			if errors.Is(err, domain.ErrNotFound) {
				saved = domain.Session{Key: item.SessionKey, Context: &item.Payload.Context}
			}
			endpoint, err := r.hub.OpenSession(item.SessionKey)
			if err != nil {
				if err := r.fail(ctx, item, err, r.config.MaxAttempts); err != nil {
					return 0, err
				}
				continue
			}
			processCtx, cancel := context.WithCancel(ctx)
			p = &process{key: item.SessionKey, token: saved.ResumeToken, cancel: cancel, inputs: make(chan domain.InboxItem, 1)}
			r.mu.Lock()
			r.live[item.SessionKey] = p
			r.mu.Unlock()
			r.workers.Add(1)
			go r.pump(processCtx, p, saved.ResumeToken, endpoint)
		}
		p.delivering = &item
		p.inputs <- item
	}
	for _, p := range r.live {
		if p.closeAtIdle && p.idle() {
			r.retire(p, "rest")
		}
	}
	return delay, nil
}

func (p *process) idle() bool {
	return !p.closing && !p.busy() && p.model != nil && p.model.Idle()
}

func (p *process) busy() bool {
	return p.delivering != nil || len(p.unstarted) != 0 || p.turn != nil
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
			if p.key.IsNotice() {
				var err error
				if r.config.NotifyOwner == nil {
					err = errors.New("owner notification destination unavailable")
				} else {
					noticeCtx, cancel := context.WithTimeout(ctx, operationTimeout)
					err = r.config.NotifyOwner(noticeCtx, item.Payload.Notice)
					cancel()
				}
				emit(result{delivered: true, skipped: true, err: err})
				return
			}
			input := item.Payload.Input
			var err error
			// Preparation must not block the pump indefinitely.
			prepareCtx, cancel := context.WithTimeout(ctx, operationTimeout)
			if r.config.Procedure != nil && item.Payload.Procedure != "" {
				input.Text, err = r.config.Procedure(prepareCtx, item.Payload.Procedure)
			}
			if err == nil && r.config.Prepare != nil {
				item.Payload.Input = input
				input, err = r.config.Prepare(prepareCtx, item)
			}
			cancel()
			if err != nil {
				emit(result{delivered: true, skipped: true, err: err})
				continue
			}
			if model == nil {
				prompt := "Post messages with the messaging send tools. Your final assistant text is private."
				if r.config.Prompt != nil {
					var extra string
					promptCtx, cancel := context.WithTimeout(ctx, operationTimeout)
					extra, err = r.config.Prompt(promptCtx, domain.ToolSession{Key: p.key})
					cancel()
					prompt += "\n\n" + extra
				}
				if err == nil {
					model, err = r.model.Open(ctx, domain.ModelSessionConfig{
						Key: p.key, ResumeToken: token, ToolHub: endpoint, Workdir: r.config.Workdir, SystemPrompt: prompt,
						ChatWritingTools: r.config.ChatWritingTools, OnCompactionDone: r.Notify,
					})
				}
				if err == nil {
					events = model.Events()
					emit(result{model: model})
				}
			}
			if err == nil {
				input.ID = item.ID
				if item.LastError != "" {
					input.Text = fmt.Sprintf("An earlier attempt at this input failed: %s. Check what it already did before you repeat a step.\n\n%s", strings.Join(strings.Fields(item.LastError), " "), input.Text)
				}
				if tokens := model.ContextTokens(); p.key == domain.MainSessionKey && tokens >= compactNudgeTokens {
					input.Text = fmt.Sprintf("Context: %dk tokens, over %dk. Call compact at your next break.\n\n%s", tokens/1000, compactNudgeTokens/1000, input.Text)
				}
				err = model.Send(ctx, input)
			}
			emit(result{delivered: true, err: err})
			if err != nil {
				if errors.Is(err, domain.ErrRejected) {
					continue
				}
				return
			}
		case event, ok := <-events:
			if !ok {
				emit(result{err: errors.New(modelProcessExited)})
				return
			}
			emit(result{event: &event})
		}
	}
}

// Writes slower than this mean that storage is stuck. The reconciler then stops,
// and recovery at restart reads the state from SQLite.
const handleWriteTimeout = 30 * time.Second

func (r *Reconciler) handle(ctx context.Context, out result) error {
	// handle also runs after shutdown cancels ctx. It must still record a delivery, or recovery sends the row again.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handleWriteTimeout)
	defer cancel()
	p := out.process
	if out.actionDone != nil {
		if r.live[p.key] != p || p.closing {
			out.actionDone <- errInactiveSession
			return nil
		}
		if out.action == compactSession {
			switch {
			case p.model == nil:
				out.actionDone <- errInactiveSession
			case p.closeAtIdle:
				out.actionDone <- errors.New("session is closing when idle")
			default:
				p.model.Compact()
				out.actionDone <- nil
			}
			return nil
		}
		p.closeAtIdle = true
		err := r.save(ctx, p)
		out.actionDone <- err
		return err
	}
	if r.live[p.key] != p {
		return nil
	}
	if out.exited {
		reason := p.reason
		if reason == "" {
			reason = modelProcessExited
		}
		if p.model != nil {
			attrs := []any{"session", p.key, "reason", reason}
			if reason == modelProcessExited && p.lastError != "" {
				attrs = append(attrs, "error", p.lastError)
			}
			slog.InfoContext(ctx, "Model process exited", attrs...)
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
		slog.InfoContext(ctx, "Model process started", "session", p.key)
		return r.save(ctx, p)
	}
	if out.delivered {
		item := p.delivering
		p.delivering = nil
		if out.err != nil {
			if p.closing {
				return nil
			}
			if out.skipped && !p.key.IsNotice() {
				return r.fail(ctx, *item, out.err, 1)
			}
			if err := r.fail(ctx, *item, out.err, r.config.MaxAttempts); err != nil {
				return err
			}
			if !errors.Is(out.err, domain.ErrRejected) || p.model == nil {
				r.retire(p, out.err.Error())
			}
			return nil
		}
		if !out.skipped && p.turn == nil {
			p.turn = &domain.SessionTurn{InputID: item.ID}
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
			if err := r.fail(ctx, item, out.event.Err, r.config.MaxAttempts); err != nil {
				return err
			}
			if p.turn != nil && p.turn.InputID == item.ID && len(p.unstarted) == 0 {
				p.turn = nil
			}
			return r.save(ctx, p)
		}
		if p.turn != nil && p.turnStarted {
			p.turn.Continuations = max(p.turn.Continuations, item.Payload.Continuations)
		}
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
	if err := r.events.SaveModel(ctx, domain.ModelRecord{SessionKey: p.key, Event: event, RecordedAt: time.Now()}); err != nil {
		return err
	}
	switch event.Kind {
	case domain.ModelEventTurnStarted:
		if p.turn == nil {
			p.turn = &domain.SessionTurn{}
			for id := range p.unstarted {
				if p.turn.InputID == 0 || id < p.turn.InputID {
					p.turn.InputID = id
				}
			}
		}
		if !p.turnStarted {
			attrs := []any{"session", p.key}
			if p.turn.InputID != 0 {
				attrs = append(attrs, "item", p.turn.InputID)
			}
			slog.InfoContext(ctx, "Turn started", attrs...)
			p.turnStarted = true
		}
	case domain.ModelEventError:
		p.lastError = event.Err.Error()
		if p.turn != nil {
			p.turn.Failure = p.lastError
		}
	case domain.ModelEventTurnFinished:
		attrs := []any{"session", p.key}
		if p.turn != nil && p.turn.Failure != "" {
			attrs = append(attrs, "error", p.turn.Failure)
			if _, err := r.inbox.Enqueue(ctx, domain.NewOwnerNotice("turn-failure:"+uuid.NewString(), fmt.Sprintf("Turn in %s failed: %s", p.key, p.turn.Failure))); err != nil {
				return err
			}
		}
		if p.turnStarted {
			slog.InfoContext(ctx, "Turn finished", attrs...)
			p.turnStarted = false
			p.lastError = ""
		}
		p.turn = nil
		if err := r.save(ctx, p); err != nil {
			return err
		}
		return nil
	}
	return r.save(ctx, p)
}
func (r *Reconciler) state(p *process) domain.Session {
	s := domain.Session{Key: p.key, ResumeToken: p.token, Context: &domain.SessionContext{Workdir: r.config.Workdir}, Turn: p.turn}
	if p.closeAtIdle {
		s.ResumeToken = ""
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
	r.hub.CloseSession(p.key)
	p.cancel()
}
func (r *Reconciler) fail(ctx context.Context, item domain.InboxItem, cause error, limit int) error {
	if errors.Is(cause, domain.ErrRejected) {
		r.Notify()
		return r.inbox.Reject(ctx, item.ID, cause.Error())
	}
	retry := time.Now().Add(r.config.RetryDelay * time.Duration(1<<min(item.Attempts, 8)))
	dead, err := r.inbox.Finish(ctx, item.ID, cause.Error(), limit, retry)
	if err != nil {
		return err
	}
	r.Notify()
	slog.WarnContext(ctx, "Inbox delivery failed", "item", item.ID, "session", item.SessionKey, "error", cause, "dead", dead)
	return nil
}
