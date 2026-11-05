package domain

import (
	"context"
	"time"
)

type EpisodeEntry struct {
	Ambiguous   bool
	Accepted    *bool
	Deleted     bool
	ID          string
	SessionKey  SessionKey
	At          time.Time
	InboundKind InboundEventKind
	Sender      Sender
	ModelKind   ModelEventKind
	Text        string
	ToolCall    *ToolCall
	ToolError   bool
	Sent        bool
	AfterSend   bool
}

type EpisodeRepository interface {
	Since(ctx context.Context, since, until time.Time) ([]EpisodeEntry, error)
	LastRun(context.Context) (time.Time, error)
	SaveLastRun(context.Context, time.Time) error
}
