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
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/motoki317/noctiluca/pkg/domain"
)

const maxFrameSize = 16 << 20

type initializationExit struct{ messages []string }

func (e *initializationExit) Error() string {
	detail := strings.Join(e.messages, "; ")
	if detail == "" {
		detail = authRemedy
	}
	return "claude exited before initialization: " + detail
}

type session struct {
	ctx          context.Context
	cancel       context.CancelFunc
	cmd          *exec.Cmd
	stdin        *os.File
	writeLock    chan struct{}
	events       chan domain.ModelEvent
	done         chan struct{}
	processDone  chan struct{}
	shutdownDone chan struct{}
	ready        chan error
	mu           sync.RWMutex
	token        string
	pending      map[string]int64
	active       bool
	background   int
	closeErr     error
	initErr      *initializationExit
}

func startSession(ctx context.Context, executable string, args []string, config domain.ModelSessionConfig, dir string) (*session, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &session{ctx: sessionCtx, cancel: cancel, writeLock: make(chan struct{}, 1), events: make(chan domain.ModelEvent, 64), done: make(chan struct{}), processDone: make(chan struct{}), shutdownDone: make(chan struct{}), ready: make(chan error, 1), token: config.ResumeToken}
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
	var p parser
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
		events := p.events(f, s.pending)
		s.active, s.background = p.active, len(p.tasks)
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

func (s *session) emit(event domain.ModelEvent) {
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

func (s *session) write(ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	select {
	case s.writeLock <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return errors.New("claude session is closed")
	}
	defer func() { <-s.writeLock }()
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
		return errors.New("write claude input failed")
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
	return len(s.pending) == 0 && !s.active && s.background == 0
}
func (s *session) Close() error { s.cancel(); <-s.done; return s.closeErr }
