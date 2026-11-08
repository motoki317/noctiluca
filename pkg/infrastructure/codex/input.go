// Codex app-server protocol (Apache-2.0): https://github.com/openai/codex.
// Independent Go implementation. See THIRD_PARTY_NOTICES.md.

package codex

import (
	"context"
	"errors"

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
			// A refused steer stays queued until turn/completed clears the current
			// turn and wakes this loop to send the input on the next turn.
			if len(s.inputs) == 0 || (s.current != nil && s.current.refused) {
				s.mu.Unlock()
				break
			}
			t := s.current
			method := "turn/steer"
			params := map[string]any{"threadId": s.threadID, "input": []map[string]string{{"type": "text", "text": s.inputs[0]}}}
			if t == nil {
				t = &runningTurn{ready: make(chan struct{})}
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
			// Shutdown still needs the turn/start reply's ID for turn/interrupt if
			// turn/started is absent. Waiting for the turn/steer reply also resolves
			// whether its input was accepted or must remain queued.
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
			t.markReady()
			s.inputs = s.inputs[1:]
			if err != nil && s.current == t {
				s.current = nil
			}
			s.mu.Unlock()
			if err != nil {
				s.emit(domain.ModelEvent{Kind: domain.ModelEventError, Err: err})
				s.emit(domain.ModelEvent{Kind: domain.ModelEventTurnFinished})
				if !errors.As(err, &rpcErr) {
					s.cancel()
					return
				}
			}
		}
	}
}
