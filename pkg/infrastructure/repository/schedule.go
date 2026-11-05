package repository

import (
	"context"
	"database/sql"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type scheduleRepository struct{ db *sql.DB }

func NewScheduleRepository(db *sql.DB) domain.ScheduleRepository { return &scheduleRepository{db: db} }

func (r *scheduleRepository) Save(ctx context.Context, schedule domain.Schedule) error {
	if schedule.Actor == "" {
		schedule.Actor = domain.ActorOwner
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO schedules(name, cron, tz, prompt, workdir, actor, next_run) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET next_run=CASE WHEN cron=excluded.cron AND tz=excluded.tz THEN next_run ELSE excluded.next_run END, cron=excluded.cron, tz=excluded.tz, prompt=excluded.prompt, workdir=excluded.workdir, actor=excluded.actor`,
		schedule.Name, schedule.Cron, schedule.TZ, schedule.Prompt, schedule.Workdir, schedule.Actor, schedule.NextRun.UTC())
	return err
}

func (r *scheduleRepository) List(ctx context.Context) ([]domain.Schedule, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT name, cron, tz, prompt, workdir, actor, next_run FROM schedules ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schedules []domain.Schedule
	for rows.Next() {
		var schedule domain.Schedule
		if err := rows.Scan(&schedule.Name, &schedule.Cron, &schedule.TZ, &schedule.Prompt, &schedule.Workdir, &schedule.Actor, &schedule.NextRun); err != nil {
			return nil, err
		}
		schedules = append(schedules, schedule)
	}
	return schedules, rows.Err()
}

func (r *scheduleRepository) Delete(ctx context.Context, name string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM schedules WHERE name=?`, name)
	return err
}
