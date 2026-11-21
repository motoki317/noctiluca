// Codex app-server protocol (Apache-2.0): https://github.com/openai/codex.
// Independent Go implementation. See THIRD_PARTY_NOTICES.md.

package codex

import (
	"context"
	"errors"
	"log/slog"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (s *session) wakeInputs() {
	select {
	case s.inputReady <- struct{}{}:
	default:
	}
}

func (s *session) sendInputs() {
	defer close(s.inputDone)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.inputReady:
		}
		for s.ctx.Err() == nil {
			s.mu.Lock()
			if s.compactRequested && s.idle() {
				t := &runningTurn{idSettled: make(chan struct{}), started: make(chan struct{}), compaction: &compaction{preTokens: s.contextTokens}}
				threadID := s.threadID
				s.current = t
				s.compactRequested = false
				s.mu.Unlock()
				s.startCompaction(t, threadID)
				continue
			}
			// A refused steer stays queued until turn/completed clears the current
			// turn and wakes this loop to send the input on the next turn.
			if len(s.inputs) == 0 || (s.current != nil && (s.current.refused || s.current.compaction != nil)) {
				s.mu.Unlock()
				break
			}
			t := s.current
			input := s.inputs[0]
			method := "turn/steer"
			params := map[string]any{"threadId": s.threadID, "input": []map[string]string{{"type": "text", "text": input.text}}}
			if t == nil {
				t = &runningTurn{idSettled: make(chan struct{}), started: make(chan struct{})}
				s.current = t
				method = "turn/start"
			} else {
				params["expectedTurnId"] = t.id
			}
			s.mu.Unlock()
			var result struct {
				Turn   turn   `json:"turn"`
				TurnID string `json:"turnId"`
			}
			// This call outlives session cancellation. If turn/started is absent, shutdown needs the
			// turn ID from the turn/start reply for turn/interrupt. The turn/steer reply tells whether
			// its input was accepted or stays queued.
			err := s.call(context.WithoutCancel(s.ctx), method, params, &result)
			s.mu.Lock()
			var rpcErr *rpcError
			if method == "turn/steer" && errors.As(err, &rpcErr) {
				t.refused = true
				s.mu.Unlock()
				continue
			}
			if err == nil {
				id := result.Turn.ID
				if method == "turn/steer" {
					id = result.TurnID
				}
				if id == "" || (t.id != "" && t.id != id) {
					err = errors.New("invalid codex response to " + method)
				} else {
					t.id = id
				}
			}
			t.settleID()
			s.inputs = s.inputs[1:]
			if err != nil && s.current == t {
				s.current = nil
			}
			s.mu.Unlock()
			if err != nil {
				event := domain.ModelEvent{Kind: domain.ModelEventError, Err: err}
				if method == "turn/start" && input.acknowledge {
					event.Kind, event.InputID = domain.ModelEventInputRefused, input.id
				}
				s.emit(event)
				s.emit(domain.ModelEvent{Kind: domain.ModelEventTurnFinished})
				if !errors.As(err, &rpcErr) {
					s.cancel()
					return
				}
			} else {
				// The ModelSession contract puts turn_started before input_started.
				select {
				case <-t.started:
				case <-s.ctx.Done():
					return
				}
				if input.acknowledge {
					s.emit(domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: input.id})
				}
			}
		}
	}
}

func (s *session) startCompaction(t *runningTurn, threadID string) {
	if err := s.call(context.WithoutCancel(s.ctx), "thread/compact/start", map[string]string{"threadId": threadID}, nil); err != nil {
		s.mu.Lock()
		t.settleID()
		released := s.current == t
		if released {
			s.current = nil
			slog.WarnContext(s.ctx, "Codex compaction failed to start", "thread", threadID)
		}
		s.mu.Unlock()
		if released && s.onCompactionDone != nil {
			s.onCompactionDone()
		}
	}
}
