package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type consolidationRepository struct{ db *sql.DB }

func NewConsolidationRepository(db *sql.DB) domain.ConsolidationRepository {
	return &consolidationRepository{db}
}

func (r *consolidationRepository) SaveLastRun(ctx context.Context, at time.Time) error {
	if at.IsZero() {
		return errors.New("consolidation time is required")
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO consolidation(id,last_run) VALUES(1,?)
 ON CONFLICT(id) DO UPDATE SET last_run=excluded.last_run WHERE excluded.last_run > consolidation.last_run`, at.UTC())
	return err
}
