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
	inputDone    chan struct{}
	inputReady   chan struct{}
	secret       []byte
	mu           sync.RWMutex
	nextID       uint64
	calls        map[string]chan rpcFrame
	threadID     string
	promptHash   string
	current      *runningTurn
	inputs       []domain.UserInput
	commands     map[string]*runningCommand
}

type runningTurn struct {
	id      string
	ready   chan struct{}
	refused bool
}

type runningCommand struct {
	turnID   string
	finished bool
}

func startSession(ctx context.Context, executable string, config domain.ModelSessionConfig) (*session, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &session{ctx: sessionCtx, cancel: cancel, writeLock: make(chan struct{}, 1), events: make(chan domain.ModelEvent, 64),
		done: make(chan struct{}), readerDone: make(chan struct{}), processDone: make(chan struct{}), shutdownDone: make(chan struct{}),
		inputDone: make(chan struct{}), inputReady: make(chan struct{}, 1),
		calls: make(map[string]chan rpcFrame), commands: make(map[string]*runningCommand)}
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
	go s.sendInputs()
	go s.read(stdout)
	return s, nil
}

func (s *session) shutdown() {
	defer close(s.shutdownDone)
	<-s.ctx.Done()
	s.mu.RLock()
	var ready chan struct{}
	if s.current != nil {
		ready = s.current.ready
	}
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
	threadID, active := s.threadID, ""
	if s.current != nil {
		active = s.current.id
	}
	s.mu.RUnlock()
	if active != "" {
		// The core cancels the process context when its turn deadline expires.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.call(ctx, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": active}, nil)
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
			select {
			case call <- f:
			default:
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
		s.mu.RLock()
		if s.threadID != "" && n.ThreadID != "" && n.ThreadID != s.threadID {
			s.mu.RUnlock()
			continue
		}
		s.mu.RUnlock()
		events, err := notificationEvents(f.Method, n)
		if err != nil {
			readErr = err
			break
		}
		s.mu.Lock()
		switch f.Method {
		case "turn/started":
			if s.current == nil {
				s.current = &runningTurn{id: n.Turn.ID}
			} else if s.current.id == "" {
				s.current.id = n.Turn.ID
				s.current.markReady()
			}
		case "turn/completed":
			if s.current != nil && s.current.id == n.Turn.ID {
				s.current = nil
			} else {
				events = nil
			}
			for _, command := range s.commands {
				if command.turnID == n.Turn.ID {
					command.finished = true
				}
			}
		}
		for i := range events {
			event := &events[i]
			if event.Kind == domain.ModelEventToolCall && event.ToolCall.Name == "commandExecution" {
				if _, exists := s.commands[event.ToolCall.ID]; !exists {
					s.commands[event.ToolCall.ID] = &runningCommand{turnID: n.TurnID}
				}
			} else if event.Kind == domain.ModelEventToolResult {
				if command := s.commands[event.ToolCall.ID]; command != nil {
					delete(s.commands, event.ToolCall.ID)
					if command.finished {
						event.Kind = domain.ModelEventBackgroundDone
					}
				}
			}
		}
		s.mu.Unlock()
		for _, event := range events {
			s.emit(event)
		}
		if f.Method == "turn/completed" {
			s.wakeInputs()
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
	<-s.inputDone
}

func (t *runningTurn) markReady() {
	if t.ready != nil {
		close(t.ready)
		t.ready = nil
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
	s.nextID++
	id := strconv.FormatUint(s.nextID, 10)
	s.calls[id] = response
	s.mu.Unlock()
	if err := s.write(ctx, rpcFrame{ID: json.RawMessage(id), Method: method, Params: data}); err != nil {
		s.mu.Lock()
		delete(s.calls, id)
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
	text := input.Text
	if len(input.Attachments) != 0 {
		refs, err := json.Marshal(input.Attachments)
		if err != nil {
			return err
		}
		text += "\n\nAttachment references (use the messaging tools to download):\n" + string(refs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ctx.Err() != nil {
		return errors.New("codex session is closed")
	}
	s.inputs = append(s.inputs, domain.UserInput{ID: input.ID, Text: text})
	s.wakeInputs()
	return nil
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
	return len(s.inputs) == 0 && s.current == nil && len(s.commands) == 0
}
func (s *session) Close() error { s.cancel(); <-s.done; return nil }
