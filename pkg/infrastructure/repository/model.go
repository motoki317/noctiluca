package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (r *eventRepository) SaveModel(ctx context.Context, record domain.ModelRecord) error {
	return saveModel(ctx, r.db, record)
}

func saveModel(ctx context.Context, db executor, record domain.ModelRecord) error {
	call, err := json.Marshal(record.Event.ToolCall)
	if err != nil {
		return err
	}
	message := ""
	if record.Event.Err != nil {
		message = record.Event.Err.Error()
	}
	_, err = db.ExecContext(ctx, `INSERT INTO model_events(session_key, kind, text, tool_call, error, recorded_at) VALUES (?, ?, ?, ?, ?, ?)`,
		record.SessionKey, record.Event.Kind, record.Event.Text, string(call), message, record.RecordedAt.UTC())
	return err
}

func (r *eventRepository) ListModel(ctx context.Context, key domain.SessionKey, limit int) ([]domain.ModelRecord, error) {
	if limit <= 0 {
		return nil, errors.New("limit must be positive")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT kind, text, tool_call, error, recorded_at FROM model_events
		WHERE session_key=? ORDER BY recorded_at DESC, id DESC LIMIT ?`, key, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []domain.ModelRecord
	for rows.Next() {
		record := domain.ModelRecord{SessionKey: key}
		var call, message string
		if err := rows.Scan(&record.Event.Kind, &record.Event.Text, &call, &message, &record.RecordedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(call), &record.Event.ToolCall); err != nil {
			return nil, err
		}
		if message != "" {
			record.Event.Err = errors.New(message)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
