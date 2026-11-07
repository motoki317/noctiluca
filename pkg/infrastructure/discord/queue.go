package discord

import (
	"context"
	"sync"

	"github.com/disgoorg/disgo/gateway"
)

// A slow inbox must not hold Disgo's read loop and prevent heartbeat ACKs.
type eventQueue struct {
	mu     sync.Mutex
	events []gateway.EventData
	ready  chan struct{}
}

func (q *eventQueue) push(ctx context.Context, event gateway.EventData) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	q.events = append(q.events, event)
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *eventQueue) take() []gateway.EventData {
	q.mu.Lock()
	defer q.mu.Unlock()
	events := q.events
	q.events = nil
	return events
}
