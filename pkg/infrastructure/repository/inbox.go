package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type inboxRepository struct{ db *sql.DB }

func NewInboxRepository(db *sql.DB) domain.InboxRepository { return &inboxRepository{db} }

func insertInbox(ctx context.Context, db executor, item domain.InboxItem) (sql.Result, error) {
	raw, err := json.Marshal(item.Payload)
	if err != nil {
		return nil, err
	}
	if item.Status == "" {
		item.Status = "pending"
	}
	// Existing databases require an actor value even though sessions have no roles.
	return db.ExecContext(ctx, `INSERT INTO inbox(source_key,session_key,actor,payload,ready_at,lane,status,retry_at,deadline) VALUES(?,?,'owner',?,?,?,?,?,?) ON CONFLICT(source_key) DO NOTHING`, item.SourceKey, item.SessionKey, string(raw), item.ReadyAt.UTC(), item.Lane, item.Status, item.RetryAt.UTC(), item.Deadline.UTC())
}
func (r *inboxRepository) Enqueue(ctx context.Context, item domain.InboxItem) (int64, error) {
	result, err := insertInbox(ctx, r.db, item)
	if err != nil {
		return 0, err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return 0, err
	}
	return result.LastInsertId()
}
func (r *inboxRepository) Receive(ctx context.Context, event domain.InboundEvent, item domain.InboxItem) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := insertInbox(ctx, tx, item)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		if err := saveInbound(ctx, tx, event); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (r *inboxRepository) Active(ctx context.Context) ([]domain.InboxItem, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,source_key,session_key,payload,ready_at,lane,status,attempts,retry_at,deadline,last_error FROM inbox WHERE status IN ('pending','dispatched') ORDER BY ready_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []domain.InboxItem
	for rows.Next() {
		var item domain.InboxItem
		var raw string
		if err := rows.Scan(&item.ID, &item.SourceKey, &item.SessionKey, &raw, &item.ReadyAt, &item.Lane, &item.Status, &item.Attempts, &item.RetryAt, &item.Deadline, &item.LastError); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &item.Payload); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
func (r *inboxRepository) Claim(ctx context.Context, id int64, deadline time.Time) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE inbox SET status='dispatched',deadline=? WHERE id=? AND status='pending'`, deadline.UTC(), id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
func (r *inboxRepository) Finish(ctx context.Context, id int64, failure string, limit int, retry time.Time) (bool, error) {
	if failure == "" {
		_, err := r.db.ExecContext(ctx, `UPDATE inbox SET status='done' WHERE id=? AND status='dispatched'`, id)
		return false, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var status, key, raw string
	var attempts int
	err = tx.QueryRowContext(ctx, `UPDATE inbox SET attempts=attempts+1,status=CASE WHEN attempts+1>=? THEN 'dead' ELSE 'pending' END,retry_at=?,last_error=? WHERE id=? AND status='dispatched' RETURNING status,session_key,payload,attempts`, limit, retry.UTC(), failure, id).Scan(&status, &key, &raw, &attempts)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status == "dead" {
		var payload domain.InboxPayload
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			return false, err
		}
		if payload.Notice == "" {
			if err := insertOwnerNotice(ctx, tx, fmt.Sprintf("failure:%d", id), fmt.Sprintf("Work item %d for %s failed after %d attempts: %s", id, key, attempts, failure)); err != nil {
				return false, err
			}
		}
	}
	return status == "dead", tx.Commit()
}

func insertOwnerNotice(ctx context.Context, db executor, source, text string) error {
	notice := domain.InboxItem{SourceKey: source, SessionKey: domain.SessionKey("notice:" + source), Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Notice: text, Context: domain.SessionContext{TaskName: "failure-notice"}}}
	_, err := insertInbox(ctx, db, notice)
	return err
}
func (r *inboxRepository) Fire(ctx context.Context, schedule domain.Schedule, next time.Time, item *domain.InboxItem) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE schedules SET next_run=? WHERE name=? AND next_run=?`, next.UTC(), schedule.Name, schedule.NextRun.UTC())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 && item != nil {
		if _, err := insertInbox(ctx, tx, *item); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type watchRepository struct{ db *sql.DB }

func NewWatchRepository(db *sql.DB) domain.WatchRepository { return &watchRepository{db} }
func (r *watchRepository) Add(ctx context.Context, w domain.Watch) (int64, error) {
	raw, err := json.Marshal(w.Item)
	if err != nil {
		return 0, err
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO watches(command,cwd,item) VALUES(?,?,?)`, w.Command, w.CWD, string(raw))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}
func (r *watchRepository) Unfinished(ctx context.Context) ([]domain.Watch, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,command,cwd,item FROM watches WHERE outcome IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var watches []domain.Watch
	for rows.Next() {
		var w domain.Watch
		var raw string
		if err := rows.Scan(&w.ID, &w.Command, &w.CWD, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &w.Item); err != nil {
			return nil, err
		}
		watches = append(watches, w)
	}
	return watches, rows.Err()
}
func (r *watchRepository) Complete(ctx context.Context, id int64, outcome string, at time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, `UPDATE watches SET outcome=? WHERE id=? AND outcome IS NULL RETURNING item`, outcome, id).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var item domain.InboxItem
	if err = json.Unmarshal([]byte(raw), &item); err != nil {
		return err
	}
	item.SourceKey = fmt.Sprintf("watch:%d", id)
	item.Payload.Input.Text = outcome
	item.ReadyAt = at
	if _, err := insertInbox(ctx, tx, item); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *inboxRepository) Delivered(ctx context.Context, id int64, session domain.Session) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE inbox SET status='done' WHERE id=? AND status='dispatched'`, id); err != nil {
		return err
	}
	if err := saveSession(ctx, tx, session); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *inboxRepository) Reject(ctx context.Context, id int64, reason string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE inbox SET status='dead',last_error=? WHERE id=? AND status IN ('pending','dispatched')`, reason, id)
	return err
}
