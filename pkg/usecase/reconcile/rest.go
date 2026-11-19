package reconcile

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type sessionAction int

const (
	restSession sessionAction = iota
	compactSession
)

var errInactiveSession = errors.New("session is no longer active")

func (r *Reconciler) rest(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if err := r.requestSessionAction(ctx, restSession); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"status":"closing when idle"}`), nil
}

func (r *Reconciler) compact(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if err := r.requestSessionAction(ctx, compactSession); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"status":"compacting after this turn"}`), nil
}

func (r *Reconciler) requestSessionAction(ctx context.Context, action sessionAction) error {
	session, ok := domain.ToolSessionFromContext(ctx)
	if !ok {
		return errors.New("session context is required")
	}
	r.mu.Lock()
	p := r.live[session.Key]
	active := p != nil && !p.closing
	r.mu.Unlock()
	if !active {
		return errInactiveSession
	}
	done := make(chan error, 1)
	select {
	case r.results <- result{process: p, action: action, actionDone: done}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
