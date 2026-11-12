package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type sessionRepository struct{ db *sql.DB }

func NewSessionRepository(db *sql.DB) domain.SessionRepository { return &sessionRepository{db: db} }

func (r *sessionRepository) Get(ctx context.Context, key domain.SessionKey) (domain.Session, error) {
	var session domain.Session
	var raw, turn string
	if err := r.db.QueryRowContext(ctx, `SELECT key, resume_token, context_json, turn_json FROM sessions WHERE key=?`, key).Scan(&session.Key, &session.ResumeToken, &raw, &turn); err != nil {
		return session, mapNotFound(err)
	}
	return session, errors.Join(json.Unmarshal([]byte(raw), &session.Context), json.Unmarshal([]byte(turn), &session.Turn))
}

func (r *sessionRepository) List(ctx context.Context) ([]domain.Session, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT key, resume_token, context_json FROM sessions ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []domain.Session
	for rows.Next() {
		var session domain.Session
		var raw string
		if err := rows.Scan(&session.Key, &session.ResumeToken, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &session.Context); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (r *sessionRepository) Save(ctx context.Context, session domain.Session) error {
	return saveSession(ctx, r.db, session)
}

func saveSession(ctx context.Context, db executor, session domain.Session) error {
	raw, err := json.Marshal(session.Context)
	if err != nil {
		return err
	}
	turn, err := json.Marshal(session.Turn)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO sessions(key, resume_token, context_json, turn_json) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET resume_token=excluded.resume_token, context_json=excluded.context_json, turn_json=excluded.turn_json`, session.Key, session.ResumeToken, string(raw), string(turn))
	return err
}

func mapNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	return err
}

func (r *sessionRepository) Recover(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `SELECT key FROM sessions WHERE turn_json!='null' UNION SELECT session_key FROM inbox WHERE status='dispatched'`)
	if err != nil {
		return err
	}
	var keys []domain.SessionKey
	for rows.Next() {
		var key domain.SessionKey
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, key)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, key := range keys {
		if err := r.Interrupt(ctx, key, "restart"); err != nil {
			return err
		}
	}
	return nil
}

func (r *sessionRepository) Interrupt(ctx context.Context, key domain.SessionKey, reason string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw, rawTurn string
	err = tx.QueryRowContext(ctx, `SELECT context_json,turn_json FROM sessions WHERE key=?`, key).Scan(&raw, &rawTurn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var info *domain.SessionContext
	var turn *domain.SessionTurn
	if raw != "" {
		if err := errors.Join(json.Unmarshal([]byte(raw), &info), json.Unmarshal([]byte(rawTurn), &turn)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE inbox SET status='pending' WHERE session_key=? AND status='dispatched'`, key); err != nil {
		return err
	}
	if turn == nil {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET turn_json='null' WHERE key=?`, key); err != nil {
		return err
	}
	started := true
	var rawInput string
	if turn.InputID != 0 {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status,payload FROM inbox WHERE id=?`, turn.InputID).Scan(&status, &rawInput); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		started = status == "done"
	}
	now := time.Now()
	note := "previous turn was interrupted: " + reason
	if err := saveModel(ctx, tx, domain.ModelRecord{SessionKey: key, RecordedAt: now, Event: domain.ModelEvent{Kind: domain.ModelEventError, Text: note, Err: errors.New(note)}}); err != nil {
		return err
	}
	if started && info != nil {
		source := fmt.Sprintf("interrupted:%s:%d", key, now.UnixNano())
		if reason == domain.TurnDeadlineExceeded || reason == domain.DeliveryDeadlineExceeded {
			if info.TaskName != "" {
				if err := insertOwnerNotice(ctx, tx, source, fmt.Sprintf("Task %s stopped: %s", info.TaskName, reason)); err != nil {
					return err
				}
			}
		} else if turn.Continuations < 1 && (info.TaskName != "" || reason != domain.ConversationNoLongerAccepted) {
			item := domain.InboxItem{SourceKey: source, SessionKey: key, Lane: "task", ReadyAt: now, Payload: domain.InboxPayload{Continuations: turn.Continuations + 1, Context: *info, Input: domain.UserInput{Text: note}}}
			if info.TaskName == "" {
				item.Lane = "conv"
				item.Payload.Input.Text += "\nThe interrupted turn may cover several inputs: finish each unfinished part at the ref its input's refs and the messaging notes call for."
				var input domain.InboxPayload
				if json.Unmarshal([]byte(rawInput), &input) == nil {
					item.Payload.Target = input.Target
					item.Payload.MessageRef = input.MessageRef
					if input.Event != nil {
						item.Payload.Target = input.Event.Conversation
						item.Payload.MessageRef = input.Event.MessageRef
					}
				}
				if item.Payload.Target == "" {
					item.Payload.Target = info.Conversation
				}
			}
			if _, err := insertInbox(ctx, tx, item); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	slog.WarnContext(ctx, "Previous turn interrupted", "session", key, "reason", reason)
	return nil
}
