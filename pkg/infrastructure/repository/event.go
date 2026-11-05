package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type eventRepository struct{ db *sql.DB }

func NewEventRepository(db *sql.DB) domain.EventRepository { return &eventRepository{db: db} }

func (r *eventRepository) SaveInbound(ctx context.Context, event domain.InboundEvent) error {
	return saveInbound(ctx, r.db, event)
}

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func saveInbound(ctx context.Context, db executor, event domain.InboundEvent) error {
	attachments, err := json.Marshal(event.Attachments)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO events
		(kind, conversation, parent, sender_id, sender_display_name, sender_kind, text, attachments, mentions_me, received_at, deleted, accepted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.Kind, event.Conversation, event.Parent, event.Sender.ID, event.Sender.DisplayName,
		event.Sender.Kind, event.Text, string(attachments), event.MentionsMe, event.ReceivedAt.UTC(), event.Deleted, event.Accepted)
	return err
}

func (r *eventRepository) SaveOutbound(ctx context.Context, message domain.SentMessage) error {
	attachments, err := json.Marshal(message.Message.Attachments)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO sends(message_id, conversation, text, attachments, sent_at) VALUES (?, ?, ?, ?, ?)`,
		message.ID, message.Conversation, message.Message.Text, string(attachments), message.SentAt.UTC())
	return err
}

const inboundColumns = `e.kind, e.conversation, e.parent, e.sender_id, e.sender_display_name,
	e.sender_kind, e.text, e.attachments, e.mentions_me, e.received_at, e.deleted, e.accepted`

func (r *eventRepository) ListInbound(ctx context.Context, conversation domain.ConversationRef, limit int) ([]domain.InboundEvent, error) {
	if limit <= 0 {
		return nil, errors.New("limit must be positive")
	}
	return r.queryInbound(ctx, `SELECT `+inboundColumns+` FROM events e
		WHERE e.conversation=? ORDER BY e.received_at DESC, e.id DESC LIMIT ?`, conversation, limit)
}

func (r *eventRepository) Search(ctx context.Context, query string, limit int) ([]domain.InboundEvent, error) {
	if limit <= 0 {
		return nil, errors.New("limit must be positive")
	}
	if query == "" {
		return nil, nil
	}
	if !utf8.ValidString(query) || strings.ContainsRune(query, 0) {
		return nil, errors.New("query must be UTF-8 text without NUL")
	}
	if utf8.RuneCountInString(query) < 3 {
		pattern := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
		return r.queryInbound(ctx, `SELECT `+inboundColumns+` FROM events e
			WHERE e.text LIKE ? ESCAPE '\' ORDER BY e.received_at DESC, e.id DESC LIMIT ?`, pattern, limit)
	}
	phrase := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
	return r.queryInbound(ctx, `SELECT `+inboundColumns+` FROM events_fts
		JOIN events e ON e.id=events_fts.rowid WHERE events_fts MATCH ?
		ORDER BY e.received_at DESC, e.id DESC LIMIT ?`, phrase, limit)
}

func (r *eventRepository) queryInbound(ctx context.Context, query string, args ...any) ([]domain.InboundEvent, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []domain.InboundEvent
	for rows.Next() {
		var event domain.InboundEvent
		var attachments string
		if err := rows.Scan(&event.Kind, &event.Conversation, &event.Parent, &event.Sender.ID, &event.Sender.DisplayName,
			&event.Sender.Kind, &event.Text, &attachments, &event.MentionsMe, &event.ReceivedAt, &event.Deleted, &event.Accepted); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(attachments), &event.Attachments); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (r *eventRepository) ListOutbound(ctx context.Context, conversation domain.ConversationRef, limit int) ([]domain.SentMessage, error) {
	if limit <= 0 {
		return nil, errors.New("limit must be positive")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT message_id, conversation, text, attachments, sent_at FROM sends
		WHERE conversation=? ORDER BY sent_at DESC, id DESC LIMIT ?`, conversation, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []domain.SentMessage
	for rows.Next() {
		var message domain.SentMessage
		var attachments string
		if err := rows.Scan(&message.ID, &message.Conversation, &message.Message.Text, &attachments, &message.SentAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(attachments), &message.Message.Attachments); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}
