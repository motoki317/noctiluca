package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/util/lifecycle"
)

type watcher struct {
	life     lifecycle.Group
	repo     domain.WatchRepository
	inbox    domain.InboxRepository
	resolve  func(context.Context, domain.SessionKey) (domain.InboxItem, error)
	notify   func()
	mu       sync.Mutex
	ctx      context.Context
	wg       sync.WaitGroup
	failures chan error
}

func New(repo domain.WatchRepository, inbox domain.InboxRepository, resolve func(context.Context, domain.SessionKey) (domain.InboxItem, error), notify func()) domain.Background {
	return &watcher{repo: repo, inbox: inbox, resolve: resolve, notify: notify, failures: make(chan error, 1)}
}

func (w *watcher) Start(ctx context.Context) error {
	ctx, finish, err := w.life.Begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		w.mu.Lock()
		w.ctx = nil
		w.mu.Unlock()
		w.wg.Wait()
	}()
	unfinished, err := w.repo.Unfinished(ctx)
	if err != nil {
		return err
	}
	for _, pending := range unfinished {
		if err := w.repo.Complete(ctx, pending.ID, fmt.Sprintf("watch %q interrupted by restart; exit unknown", pending.Command), time.Now()); err != nil {
			return err
		}
	}
	w.notify()
	w.mu.Lock()
	w.ctx = ctx
	w.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil
	case err := <-w.failures:
		return err
	}
}

func (w *watcher) complete(ctx context.Context, id int64, outcome string) error {
	err := w.repo.Complete(ctx, id, outcome, time.Now())
	if err != nil {
		select {
		case w.failures <- err:
		default:
		}
		return err
	}
	w.notify()
	return nil
}

func (w *watcher) Tools() []domain.Tool {
	return []domain.Tool{
		{Name: "wake_at", Description: "Schedule a note at an RFC3339 time, then end this turn. Copy Source.work_target exactly into target. The completion input carries this destination in its Source header.", InputSchema: json.RawMessage(`{"type":"object","properties":{"target":{"type":"string","minLength":1,"description":"Copy Source.work_target exactly. Top-level DM work stays at top level. Existing threads stay threaded."},"at":{"type":"string"},"note":{"type":"string","minLength":1}},"required":["target","at","note"],"additionalProperties":false}`), Handler: w.wake},
		{Name: "watch", Description: "Start a shell command and end this turn immediately. Its exit code and output tail return as a new input. Copy Source.work_target exactly into target. The completion input carries this destination in its Source header.", InputSchema: json.RawMessage(`{"type":"object","properties":{"target":{"type":"string","minLength":1,"description":"Copy Source.work_target exactly. Top-level DM work stays at top level. Existing threads stay threaded."},"command":{"type":"string","minLength":1},"cwd":{"type":"string","minLength":1}},"required":["target","command","cwd"],"additionalProperties":false}`), Handler: w.watch},
	}
}

func (w *watcher) wake(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	session, ok := domain.ToolSessionFromContext(ctx)
	if !ok {
		return nil, errors.New("tool session is required")
	}
	var in struct {
		Target domain.ConversationRef `json:"target"`
		At     time.Time              `json:"at"`
		Note   string                 `json:"note"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	if in.At.IsZero() || in.Note == "" || in.Target == "" {
		return nil, errors.New("target, at and note are required")
	}
	item, err := w.resolve(ctx, session.Key)
	if err != nil {
		return nil, err
	}
	item.Payload.Target = in.Target
	item.SourceKey = "wake:" + uuid.NewString()
	item.ReadyAt = in.At
	item.Payload.Input.Text = in.Note
	id, err := w.inbox.Enqueue(ctx, item)
	if err != nil {
		return nil, err
	}
	w.notify()
	return json.Marshal(map[string]int64{"id": id})
}

func (w *watcher) watch(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	session, ok := domain.ToolSessionFromContext(ctx)
	if !ok {
		return nil, errors.New("tool session is required")
	}
	var in struct {
		Target  domain.ConversationRef `json:"target"`
		Command string                 `json:"command"`
		CWD     string                 `json:"cwd"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	if in.Command == "" || in.CWD == "" || in.Target == "" {
		return nil, errors.New("target, command and cwd are required")
	}
	item, err := w.resolve(ctx, session.Key)
	if err != nil {
		return nil, err
	}
	item.Payload.Target = in.Target
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx == nil || w.ctx.Err() != nil {
		return nil, errors.New("watch service is not running")
	}
	id, err := w.repo.Add(ctx, domain.Watch{Command: in.Command, CWD: in.CWD, Item: item})
	if err != nil {
		return nil, err
	}
	lifetime := w.ctx
	cmd := exec.CommandContext(lifetime, "sh", "-c", in.Command)
	cmd.Dir = in.CWD
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	kill := func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	cmd.Cancel = kill
	// A descendant must not keep output pipes open after the command exits.
	cmd.WaitDelay = 100 * time.Millisecond
	output := &tail{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, w.complete(lifetime, id, "watch failed to start: "+err.Error()))
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		err := cmd.Wait()
		_ = kill()
		if lifetime.Err() != nil {
			return
		}
		code := cmd.ProcessState.ExitCode()
		if err != nil && code == 0 {
			code = -1
		}
		_ = w.complete(lifetime, id, fmt.Sprintf("watch exited with code %d\n%s", code, output.String()))
	}()
	return json.Marshal(map[string]int{"pid": cmd.Process.Pid})
}

type tail struct {
	mu   sync.Mutex
	data []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if len(p) >= 4096 {
		t.data = append(t.data[:0], p[len(p)-4096:]...)
	} else {
		if extra := len(t.data) + len(p) - 4096; extra > 0 {
			copy(t.data, t.data[extra:])
			t.data = t.data[:len(t.data)-extra]
		}
		t.data = append(t.data, p...)
	}
	return n, nil
}
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}

func (w *watcher) Shutdown(ctx context.Context) error { return w.life.Shutdown(ctx) }
