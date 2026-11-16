package reconcile

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (r *Reconciler) rest(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	session, ok := domain.ToolSessionFromContext(ctx)
	if !ok {
		return nil, errors.New("session context is required")
	}
	r.mu.Lock()
	p := r.live[session.Key]
	active := p != nil && !p.closing
	r.mu.Unlock()
	if !active {
		return nil, errors.New("session is no longer active")
	}
	done := make(chan error, 1)
	select {
	case r.results <- result{process: p, rest: done}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case err := <-done:
		if err != nil {
			return nil, err
		}
		return json.RawMessage(`{"status":"closing when idle"}`), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
