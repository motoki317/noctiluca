// Claude CLI protocol reference: OpenClaw (MIT), https://github.com/openclaw/openclaw.
// Go reimplementation. Provenance and license notice: THIRD_PARTY_NOTICES.md.

package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/motoki317/noctiluca/pkg/domain"
)

const (
	maxFrameSize           = 16 << 20
	idleCompactDelay       = 54 * time.Minute
	idleCompactMinTokens   = 200000
	idleCompactMaxLateness = time.Minute
)

var errWriteInput = errors.New("write claude input failed")

type initializationExit struct{ messages []string }

func (e *initializationExit) Error() string {
	detail := strings.Join(e.messages, "; ")
	if detail == "" {
		detail = authRemedy
	}
	return "claude exited before initialization: " + detail
}

type session struct {
	ctx           context.Context
	cancel        context.CancelFunc
	cmd           *exec.Cmd
	stdin         *os.File
	writeLock     chan struct{}
	events        chan domain.ModelEvent
	done          chan struct{}
	processDone   chan struct{}
	shutdownDone  chan struct{}
	ready         chan error
	mu            sync.RWMutex
	token         string
	pending       map[string]int64
	parser        parser
	compactWake   chan struct{}
	compactDone   chan struct{}
	compactAt     time.Time
	compactDelay  time.Duration
	now           func() time.Time
	lastMessageID string
	contextTokens int
	oneHourCache  bool
	closeErr      error
	initErr       *initializationExit
}

func startSession(ctx context.Context, executable string, args []string, config domain.ModelSessionConfig, dir string) (*session, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &session{ctx: sessionCtx, cancel: cancel, writeLock: make(chan struct{}, 1), events: make(chan domain.ModelEvent, 64), done: make(chan struct{}), processDone: make(chan struct{}), shutdownDone: make(chan struct{}), ready: make(chan error, 1), token: config.ResumeToken}
	s.compactWake, s.compactDone = make(chan struct{}, 1), make(chan struct{})
	s.compactDelay, s.now = idleCompactDelay, time.Now
	cmd := exec.Command(executable, args...)
	cmd.Dir, cmd.Env = config.Workdir, childEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.cmd = cmd
	input, output, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create claude stdin: %w", err)
	}
	s.stdin, cmd.Stdin = output, input
	// Own the pipe so Wait can observe exit without closing unread output.
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		cancel()
		_ = input.Close()
		_ = output.Close()
		return nil, fmt.Errorf("create claude stdout: %w", err)
	}
	cmd.Stdout = stdoutWriter
	// Nil stderr uses the null device, with no copier waiting on descendants.
	// Raw stderr can contain credentials or tool output, so it never reaches logs.
	if err := cmd.Start(); err != nil {
		cancel()
		_ = input.Close()
		_ = output.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return nil, errors.New("start claude failed")
	}
	_ = input.Close()
	_ = stdoutWriter.Close()
	go func() {
		_ = cmd.Wait()
		// Descendants can retain stdout and session credentials after their parent exits.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		close(s.processDone)
	}()
	go s.compactIdle()
	go s.shutdown()
	go s.read(stdout, dir)
	initCtx, initCancel := context.WithTimeout(ctx, 10*time.Second)
	defer initCancel()
	err = s.write(initCtx, map[string]any{"type": "control_request", "request_id": "initialize", "request": map[string]string{"subtype": "initialize"}})
	if err != nil && initCtx.Err() == nil {
		err = &initializationExit{}
	}
	if err == nil {
		select {
		case err = <-s.ready:
		case <-s.done:
			err = &initializationExit{}
		case <-initCtx.Done():
			err = initCtx.Err()
		}
	}
	if err != nil {
		_ = s.Close()
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if s.initErr != nil {
			err = s.initErr
		}
		return nil, fmt.Errorf("initialize claude: %w", err)
	}
	return s, nil
}

func (s *session) shutdown() {
	defer close(s.shutdownDone)
	<-s.ctx.Done()
	_ = s.stdin.Close()
	<-s.compactDone
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-s.processDone:
	case <-timer.C:
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	}
}

func (s *session) read(stdout io.ReadCloser, dir string) {
	defer close(s.done)
	defer close(s.events)
	defer stdout.Close()
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64<<10), maxFrameSize)
	var readErr error
	initialized := false
	for scan.Scan() {
		if len(scan.Bytes()) == 0 {
			continue
		}
		f, err := parseFrame(scan.Bytes())
		if err != nil {
			readErr = err
			break
		}
		if f.SessionID != "" && (f.Type == "system" && f.Subtype == "init" || f.Type == "result") {
			s.mu.Lock()
			s.token = f.SessionID
			s.mu.Unlock()
		}
		if f.Type == "control_response" && f.Response.RequestID == "initialize" {
			var err error
			if f.Response.Subtype != "success" {
				err = errors.New("claude rejected initialization")
			} else {
				initialized = true
			}
			select {
			case s.ready <- err:
			default:
			}
		}
		if !initialized && f.failedResult() {
			s.initErr = &initializationExit{messages: f.Errors}
		}
		if f.Type == "control_request" {
			response := map[string]any{"subtype": "error", "request_id": f.RequestID, "error": "Unsupported control request"}
			if f.Request.Subtype == "can_use_tool" {
				// Accepted sessions share the Unix user and its native tool permissions.
				response = map[string]any{"subtype": "success", "request_id": f.RequestID, "response": map[string]any{"behavior": "allow", "updatedInput": f.Request.Input}}
			}
			controlCtx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
			err := s.write(controlCtx, map[string]any{"type": "control_response", "response": response})
			cancel()
			if err != nil {
				readErr = err
				break
			}
		}
		s.mu.Lock()
		s.observeRequest(f)
		if s.parser.compacting && f.Type == "system" {
			if f.Subtype == "compact_boundary" {
				s.contextTokens = f.CompactMetadata.PostTokens
				slog.InfoContext(s.ctx, "Claude idle compaction completed", "session", s.token,
					"pre_tokens", f.CompactMetadata.PreTokens, "post_tokens", f.CompactMetadata.PostTokens)
			}
			if f.Subtype == "status" && f.CompactResult == "failed" {
				slog.WarnContext(s.ctx, "Claude idle compaction failed", "session", s.token)
			}
		}
		events := s.parser.events(f, s.pending)
		s.mu.Unlock()
		for _, event := range events {
			s.emit(event)
		}
		// An initialization write can fail before the CLI's final diagnostic is read.
		if initialized && s.ctx.Err() != nil {
			break
		}
	}
	if s.ctx.Err() == nil {
		if readErr == nil && scan.Err() != nil {
			readErr = errors.New("read claude stream failed")
		}
		if readErr == nil {
			readErr = errors.New("claude process exited unexpectedly: " + authRemedy)
		}
		s.emit(domain.ModelEvent{Kind: domain.ModelEventError, Err: readErr})
	}
	s.cancel()
	<-s.processDone
	<-s.shutdownDone
	s.closeErr = os.RemoveAll(dir)
}

func (s *session) observeRequest(f frame) {
	if f.ParentToolUseID != "" || s.parser.compacting {
		return
	}
	msg := f.Message
	var request bool
	switch {
	case f.Type == "stream_event" && f.Event.Type == "message_start":
		msg, request = f.Event.Message, true
	case f.Type == "assistant":
		request = msg.ID == "" || msg.ID != s.lastMessageID
	default:
		return
	}
	if u := msg.Usage; u != nil {
		s.contextTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
		if u.CacheCreationInputTokens > 0 || u.CacheCreation.Ephemeral1HInputTokens > 0 || u.CacheCreation.Ephemeral5MInputTokens > 0 {
			s.oneHourCache = u.CacheCreation.Ephemeral1HInputTokens > 0
		}
	}
	if request {
		s.lastMessageID = msg.ID
		// Host sleep must count against the cache lifetime, so discard the monotonic clock.
		s.compactAt = s.now().Round(0).Add(s.compactDelay)
		select {
		case s.compactWake <- struct{}{}:
		default:
		}
	}
}

func (s *session) compactIdle() {
	defer close(s.compactDone)
	timer := time.NewTimer(idleCompactDelay)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.compactWake:
			s.mu.Lock()
			if !s.compactAt.IsZero() {
				timer.Reset(s.compactAt.Sub(s.now().Round(0)))
			}
			s.mu.Unlock()
		case <-timer.C:
			if remaining := s.compactIfIdle(); remaining > 0 {
				timer.Reset(remaining)
			}
		}
	}
}

func (s *session) compactIfIdle() time.Duration {
	// Reserve stdin before the idle check so a concurrent Send cannot overtake /compact.
	select {
	case s.writeLock <- struct{}{}:
	case <-s.ctx.Done():
		return 0
	}
	defer func() { <-s.writeLock }()
	s.mu.Lock()
	deadline := s.compactAt
	now := s.now().Round(0)
	if deadline.IsZero() {
		s.mu.Unlock()
		return 0
	}
	if now.Before(deadline) {
		s.mu.Unlock()
		return deadline.Sub(now)
	}
	s.compactAt = time.Time{}
	if s.ctx.Err() != nil || now.Sub(deadline) > idleCompactMaxLateness || !s.idle() || s.contextTokens < idleCompactMinTokens || !s.oneHourCache {
		s.mu.Unlock()
		return 0
	}
	id := uuid.NewString()
	s.parser.compactUUID = id
	value := map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": "/compact"}, "parent_tool_use_id": nil, "uuid": id, "session_id": s.token}
	s.mu.Unlock()
	if err := s.writeLocked(s.ctx, value); errors.Is(err, errWriteInput) {
		slog.WarnContext(s.ctx, "Write Claude idle compaction failed", "error", err)
	}
	return 0
}

func (s *session) emit(event domain.ModelEvent) {
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

func (s *session) write(ctx context.Context, value any) error {
	select {
	case s.writeLock <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return errors.New("claude session is closed")
	}
	defer func() { <-s.writeLock }()
	return s.writeLocked(ctx, value)
}

func (s *session) writeLocked(ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ctx.Err() != nil {
		return errors.New("claude session is closed")
	}
	_ = s.stdin.SetWriteDeadline(time.Time{})
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = s.stdin.SetWriteDeadline(time.Now()); close(stopped) })
	_, err = s.stdin.Write(append(data, '\n'))
	if !stop() {
		<-stopped
	}
	if err != nil {
		ctxErr := ctx.Err()
		s.cancel() // A partial line cannot be retried on the same stream.
		if ctxErr != nil {
			return ctxErr
		}
		return errWriteInput
	}
	return nil
}

func (s *session) Send(ctx context.Context, input domain.UserInput) error {
	// Attachment references remain opaque. The model downloads them through adapter tools.
	text := input.Text
	if len(input.Attachments) > 0 {
		refs, err := json.Marshal(input.Attachments)
		if err != nil {
			return err
		}
		text += "\n\nAttachment references (use the messaging tools to download):\n" + string(refs)
	}
	id := uuid.NewString()
	value := map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": text}, "parent_tool_use_id": nil, "uuid": id}
	if token := s.ResumeToken(); token != "" {
		value["session_id"] = token
	}
	s.mu.Lock()
	if s.pending == nil {
		s.pending = make(map[string]int64)
	}
	s.pending[id] = input.ID
	s.mu.Unlock()
	if err := s.write(ctx, value); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *session) Events() <-chan domain.ModelEvent { return s.events }
func (s *session) ResumeToken() string              { s.mu.RLock(); defer s.mu.RUnlock(); return s.token }
func (s *session) Idle() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idle()
}
func (s *session) idle() bool {
	return len(s.pending) == 0 && !s.parser.active && len(s.parser.tasks) == 0 && s.parser.compactUUID == ""
}
func (s *session) Close() error { s.cancel(); <-s.done; return s.closeErr }
