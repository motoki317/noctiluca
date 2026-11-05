package lifecycle

import (
	"context"
	"errors"
	"sync"
)

// Group retains one lifetime across startup phases and joins them on shutdown.
type Group struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	active  int
	stopped bool
	done    chan struct{}
}

func (g *Group) Begin(parent context.Context) (context.Context, func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return nil, nil, errors.New("service is shut down")
	}
	if g.ctx == nil {
		g.ctx, g.cancel = context.WithCancel(parent)
	}
	g.active++
	stop := context.AfterFunc(parent, g.cancel)
	var once sync.Once
	return g.ctx, func() {
		once.Do(func() {
			stop()
			g.mu.Lock()
			defer g.mu.Unlock()
			g.active--
			if g.stopped && g.active == 0 {
				close(g.done)
			}
		})
	}, nil
}

func (g *Group) Shutdown(ctx context.Context) error {
	g.mu.Lock()
	if !g.stopped {
		g.stopped = true
		g.done = make(chan struct{})
		if g.cancel != nil {
			g.cancel()
		}
		if g.active == 0 {
			close(g.done)
		}
	}
	done := g.done
	g.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
