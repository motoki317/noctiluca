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

const maxContinuations = 1
const continuationInstructions = "The interrupted turn may cover several inputs. Finish each unfinished part, following the posting instructions in its input."

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
	if _, err := r.db.ExecContext(ctx, `UPDATE inbox SET status='pending' WHERE status='dispatched' AND session_key GLOB ?`, domain.NoticeSessionPrefix+"*"); err != nil {
		return err
	}
	return r.Interrupt(ctx, domain.MainSessionKey, "restart")
}

func (r *sessionRepository) Interrupt(ctx context.Context, key domain.SessionKey, reason string) error {
	if key.IsNotice() {
		_, err := r.db.ExecContext(ctx, `UPDATE inbox SET status='pending' WHERE session_key=? AND status='dispatched'`, key)
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rawTurn string
	err = tx.QueryRowContext(ctx, `SELECT turn_json FROM sessions WHERE key=?`, key).Scan(&rawTurn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var turn *domain.SessionTurn
	if rawTurn != "" {
		if err := json.Unmarshal([]byte(rawTurn), &turn); err != nil {
			return err
		}
	}
	// A dispatched row never started. It returns to pending with attempts unchanged, because the row did not fail.
	if _, err := tx.ExecContext(ctx, `UPDATE inbox SET status='pending' WHERE status='dispatched' AND session_key NOT GLOB ?`, domain.NoticeSessionPrefix+"*"); err != nil {
		return err
	}
	if turn == nil {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET turn_json='null' WHERE key=?`, key); err != nil {
		return err
	}
	openingInputStarted := true
	if turn.InputID != 0 {
		var status domain.InboxStatus
		if err := tx.QueryRowContext(ctx, `SELECT status FROM inbox WHERE id=?`, turn.InputID).Scan(&status); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		openingInputStarted = status == domain.InboxDone
	}
	now := time.Now()
	note := "previous turn was interrupted: " + reason
	if err := saveModel(ctx, tx, domain.ModelRecord{SessionKey: key, RecordedAt: now, Event: domain.ModelEvent{Kind: domain.ModelEventError, Text: note, Err: errors.New(note)}}); err != nil {
		return err
	}
	// A started input is not sent again. The continuation asks the model to finish it instead.
	if openingInputStarted && turn.Continuations < maxContinuations {
		source := fmt.Sprintf("interrupted:%s:%d", key, now.UnixNano())
		item := domain.InboxItem{SourceKey: source, SessionKey: domain.MainSessionKey, Lane: domain.LaneConversation, ReadyAt: now, Payload: domain.InboxPayload{Continuations: turn.Continuations + 1, Input: domain.UserInput{Text: note + "\n" + continuationInstructions}}}
		if _, err := insertInbox(ctx, tx, item); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	slog.WarnContext(ctx, "Previous turn interrupted", "session", key, "reason", reason)
	return nil
}
