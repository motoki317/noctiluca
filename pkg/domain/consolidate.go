package domain

import (
	"context"
	"time"
)

type ConsolidationRepository interface {
	SaveLastRun(context.Context, time.Time) error
}
