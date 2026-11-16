package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (r *eventRepository) InputContext(ctx context.Context, key domain.SessionKey, conversation domain.ConversationRef, at time.Time) (domain.InputContext, error) {
	var info domain.InputContext
	root, err := r.GetInbound(ctx, conversation)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return info, err
	}
	if err == nil {
		info.Root = &root
	}
	err = r.db.QueryRowContext(ctx, `SELECT sent_at FROM sends WHERE conversation=? ORDER BY sent_at DESC, id DESC LIMIT 1`, conversation).Scan(&info.LastReply)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return info, err
	}
	var kind domain.ModelEventKind
	err = r.db.QueryRowContext(ctx, `SELECT kind FROM model_events WHERE session_key=? AND recorded_at<=?
		AND kind IN ('turn_started','turn_finished','error') ORDER BY recorded_at DESC, id DESC LIMIT 1`, key, at.UTC()).Scan(&kind)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return info, err
	}
	info.Running = kind == domain.ModelEventTurnStarted
	return info, nil
}
