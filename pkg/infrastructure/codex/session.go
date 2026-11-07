// Codex app-server protocol (Apache-2.0): https://github.com/openai/codex.
// Independent Go implementation. See THIRD_PARTY_NOTICES.md.

package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

const maxFrameSize = 16 << 20

type session struct {
	ctx          context.Context
	cancel       context.CancelFunc
	cmd          *exec.Cmd
	stdin        *os.File
	writeLock    chan struct{}
	events       chan domain.ModelEvent
	done         chan struct{}
	readerDone   chan struct{}
	processDone  chan struct{}
	shutdownDone chan struct{}
	secret       []byte
	mu           sync.RWMutex
	nextID       uint64
	calls        map[string]chan rpcFrame
	threadID     string
	promptHash   string
	active       string
	pending      bool
	startID      string
	turnReady    chan struct{}
}

func startSession(ctx context.Context, executable string, config domain.ModelSessionConfig) (*session, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &session{ctx: sessionCtx, cancel: cancel, writeLock: make(chan struct{}, 1), events: make(chan domain.ModelEvent, 64),
		done: make(chan struct{}), readerDone: make(chan struct{}), processDone: make(chan struct{}), shutdownDone: make(chan struct{}),
		calls: make(map[string]chan rpcFrame)}
	if config.ToolHub.BearerToken != "" {
		encoded, _ := json.Marshal(config.ToolHub.BearerToken)
		s.secret = encoded[1 : len(encoded)-1]
	}
	// Empty tables replace inherited servers, including servers supplied by plugins.
	cmd := exec.Command(executable, "app-server", "-c", "mcp_servers={}", "-c", "plugins={}")
	cmd.Dir = config.Workdir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.cmd = cmd
	input, output, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create codex stdin: %w", err)
	}
	s.stdin, cmd.Stdin = output, input
	// Own stdout so Wait cannot close unread responses.
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		cancel()
		_ = input.Close()
		_ = output.Close()
		return nil, fmt.Errorf("create codex stdout: %w", err)
	}
	cmd.Stdout = stdoutWriter
	// Nil stderr discards diagnostics that can contain credentials or tool output.
	if err := cmd.Start(); err != nil {
		cancel()
		_ = input.Close()
		_ = output.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return nil, errors.New("start codex failed")
	}
	_ = input.Close()
	_ = stdoutWriter.Close()
	go func() {
		_ = cmd.Wait()
		// Descendants can retain pipes and credentials after the app-server exits.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		close(s.processDone)
	}()
	go s.shutdown()
	go s.read(stdout)
	return s, nil
}

func (s *session) shutdown() {
	defer close(s.shutdownDone)
	<-s.ctx.Done()
	s.mu.RLock()
	ready := s.turnReady
	s.mu.RUnlock()
	if ready != nil {
		timer := time.NewTimer(time.Second)
		select {
		case <-ready:
		case <-s.readerDone:
		case <-timer.C:
		}
		timer.Stop()
	}
	s.mu.RLock()
	token, active := s.threadID, s.active
	s.mu.RUnlock()
	if active != "" {
		// The core cancels the process context when its turn deadline expires.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.call(ctx, "turn/interrupt", map[string]string{"threadId": token, "turnId": active}, nil)
		cancel()
	}
	_ = s.stdin.Close()
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-s.processDone:
	case <-timer.C:
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.processDone
	}
}

func (s *session) read(stdout io.ReadCloser) {
	defer close(s.done)
	defer close(s.events)
	defer stdout.Close()
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64<<10), maxFrameSize)
	var readErr error
	for scan.Scan() {
		data := scan.Bytes()
		if len(data) == 0 {
			continue
		}
		if len(s.secret) != 0 {
			data = bytes.ReplaceAll(data, s.secret, []byte("[redacted]"))
		}
		var f rpcFrame
		if json.Unmarshal(data, &f) != nil || (f.Method == "" && len(f.ID) == 0) {
			readErr = errors.New("invalid codex stream frame")
			break
		}
		if f.Method == "" {
			s.mu.Lock()
			call, found := s.calls[string(f.ID)]
			delete(s.calls, string(f.ID))
			s.mu.Unlock()
			if !found {
				continue
			}
			if call != nil {
				select {
				case call <- f:
				default:
				}
			} else {
				s.turnResponse(f)
			}
			continue
		}
		if len(f.ID) != 0 {
			ctx, cancel := context.WithTimeout(s.ctx, time.Second)
			err := s.write(ctx, rpcFrame{ID: f.ID, Error: &rpcError{Code: -32601, Message: "Unsupported server request"}})
			cancel()
			if err != nil {
				readErr = err
				break
			}
			continue
		}
		switch f.Method {
		case "turn/started", "turn/completed", "item/agentMessage/delta", "item/started", "item/completed", "error":
		default:
			continue
		}
		var n notification
		if json.Unmarshal(f.Params, &n) != nil {
			readErr = errors.New("invalid codex notification")
			break
		}
		s.mu.Lock()
		if s.threadID != "" && n.ThreadID != "" && n.ThreadID != s.threadID {
			s.mu.Unlock()
			continue
		}
		switch f.Method {
		case "turn/started":
			s.active = n.Turn.ID
			s.markTurnReady()
		case "turn/completed":
			if s.active == n.Turn.ID {
				s.active, s.pending = "", false
			}
		}
		s.mu.Unlock()
		events, err := notificationEvents(f.Method, n)
		if err != nil {
			readErr = err
			break
		}
		for _, event := range events {
			s.emit(event)
		}
	}
	if s.ctx.Err() == nil {
		if readErr == nil && scan.Err() != nil {
			readErr = errors.New("read codex stream failed")
		}
		if readErr == nil {
			readErr = errors.New("codex process exited unexpectedly; check `codex login status`")
		}
		s.emit(domain.ModelEvent{Kind: domain.ModelEventError, Err: readErr})
	}
	close(s.readerDone)
	s.cancel()
	<-s.shutdownDone
}

func (s *session) markTurnReady() {
	if s.turnReady != nil {
		close(s.turnReady)
		s.turnReady = nil
	}
}

func (s *session) turnResponse(f rpcFrame) {
	var result struct {
		Turn turn `json:"turn"`
	}
	var err error
	if f.Error != nil {
		err = f.Error
	} else if json.Unmarshal(f.Result, &result) != nil || result.Turn.ID == "" {
		err = errors.New("invalid codex response to turn/start")
		s.cancel()
	}
	s.mu.Lock()
	if string(f.ID) == s.startID {
		if err != nil {
			s.pending = false
		} else if s.pending {
			// A completed notification can precede the response to turn/start.
			s.active = result.Turn.ID
		}
		s.markTurnReady()
	}
	s.mu.Unlock()
	if err != nil {
		s.emit(domain.ModelEvent{Kind: domain.ModelEventError, Err: err})
		s.emit(domain.ModelEvent{Kind: domain.ModelEventTurnFinished})
	}
}

func (s *session) emit(event domain.ModelEvent) {
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

func (s *session) call(ctx context.Context, method string, params, result any) error {
	response := make(chan rpcFrame, 1)
	id, err := s.request(ctx, method, params, response)
	if err != nil {
		return err
	}
	defer func() { s.mu.Lock(); delete(s.calls, id); s.mu.Unlock() }()
	select {
	case f := <-response:
		if f.Error != nil {
			return f.Error
		}
		if result != nil && json.Unmarshal(f.Result, result) != nil {
			return errors.New("invalid codex response to " + method)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.readerDone:
		return errors.New("codex stream closed during " + method)
	}
}

func (s *session) request(ctx context.Context, method string, params any, response chan rpcFrame) (string, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if response == nil && (s.pending || s.active != "") {
		s.mu.Unlock()
		return "", fmt.Errorf("codex turn is already running: %w", domain.ErrRejected)
	}
	s.nextID++
	id := strconv.FormatUint(s.nextID, 10)
	s.calls[id] = response
	if response == nil {
		s.pending, s.startID = true, id
		s.turnReady = make(chan struct{})
	}
	s.mu.Unlock()
	if err := s.write(ctx, rpcFrame{ID: json.RawMessage(id), Method: method, Params: data}); err != nil {
		s.mu.Lock()
		delete(s.calls, id)
		if response == nil {
			s.pending = false
			s.markTurnReady()
		}
		s.mu.Unlock()
		return "", err
	}
	return id, nil
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
	}
	defer func() { <-s.writeLock }()
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = s.stdin.SetWriteDeadline(time.Time{})
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = s.stdin.SetWriteDeadline(time.Now()); close(stopped) })
	_, err = s.stdin.Write(append(data, '\n'))
	if !stop() {
		<-stopped
	}
	if err != nil {
		s.cancel() // A partial frame cannot be retried on this stream.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("write codex input failed")
	}
	return nil
}

func (s *session) Send(ctx context.Context, input domain.UserInput) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ctx.Err() != nil {
		return errors.New("codex session is closed")
	}
	text := input.Text
	if len(input.Attachments) != 0 {
		refs, err := json.Marshal(input.Attachments)
		if err != nil {
			return err
		}
		text += "\n\nAttachment references (use the messaging tools to download):\n" + string(refs)
	}
	_, err := s.request(ctx, "turn/start", map[string]any{"threadId": s.threadID, "input": []map[string]string{{"type": "text", "text": text}}}, nil)
	return err
}

func (s *session) Events() <-chan domain.ModelEvent { return s.events }
func (s *session) ResumeToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return formatResumeToken(s.threadID, s.promptHash)
}
func (s *session) Idle() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.pending && s.active == ""
}
func (s *session) Close() error { s.cancel(); <-s.done; return nil }
