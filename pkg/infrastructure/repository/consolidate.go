package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type episodeRepository struct{ db *sql.DB }

func NewEpisodeRepository(db *sql.DB) domain.EpisodeRepository { return &episodeRepository{db} }

func (r *episodeRepository) LastRun(ctx context.Context) (time.Time, error) {
	var at time.Time
	err := r.db.QueryRowContext(ctx, `SELECT last_run FROM consolidation WHERE id=1`).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return at, err
}

func (r *episodeRepository) SaveLastRun(ctx context.Context, at time.Time) error {
	if at.IsZero() {
		return errors.New("consolidation time is required")
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO consolidation(id,last_run) VALUES(1,?)
 ON CONFLICT(id) DO UPDATE SET last_run=excluded.last_run WHERE excluded.last_run > consolidation.last_run`, at.UTC())
	return err
}

func (r *episodeRepository) Since(ctx context.Context, since, until time.Time) ([]domain.EpisodeEntry, error) {
	if !until.After(since) {
		return nil, errors.New("episode window must end after its start")
	}
	rows, err := r.db.QueryContext(ctx, `
 SELECT source, id, session_key, at, kind, sender_id, sender_kind, text, tool_call, error, after_send, deleted, accepted FROM (
 SELECT 'event' AS source, e.id, e.conversation AS session_key, e.received_at AS at, e.kind,
 e.sender_id, e.sender_kind, e.text, 'null' AS tool_call, '' AS error,
 COALESCE((SELECT s.sent_at FROM sends s WHERE s.conversation=e.conversation AND s.sent_at<=e.received_at
 ORDER BY s.sent_at DESC, s.id DESC LIMIT 1), '') >
 COALESCE((SELECT p.received_at FROM events p WHERE p.conversation=e.conversation
 AND (p.received_at,p.id)<(e.received_at,e.id) ORDER BY p.received_at DESC,p.id DESC LIMIT 1), '') AS after_send, e.deleted, e.accepted
 FROM events e WHERE e.received_at>=? AND e.received_at<?
 UNION ALL
 SELECT 'send', id, conversation, sent_at, '', '', '', text, 'null', '', 0, 0, NULL
 FROM sends WHERE sent_at>=? AND sent_at<?
 UNION ALL
 SELECT 'model', id, session_key, recorded_at, kind, '', '', text, tool_call, error, 0, 0, NULL
 FROM model_events WHERE recorded_at>=? AND recorded_at<? AND session_key!='task:consolidate'
 ) ORDER BY at, source, id`, since.UTC(), until.UTC(), since.UTC(), until.UTC(), since.UTC(), until.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []domain.EpisodeEntry{}
	for rows.Next() {
		var entry domain.EpisodeEntry
		var source, kind, call string
		var id int64
		if err := rows.Scan(&source, &id, &entry.SessionKey, &entry.At, &kind, &entry.Sender.ID,
			&entry.Sender.Kind, &entry.Text, &call, &entry.Error, &entry.AfterSend, &entry.Deleted, &entry.Accepted); err != nil {
			return nil, err
		}
		entry.ID = fmt.Sprintf("%s:%d", source, id)
		switch source {
		case "event":
			entry.InboundKind = domain.InboundEventKind(kind)
		case "send":
			entry.Sent = true
		case "model":
			entry.ModelKind = domain.ModelEventKind(kind)
		}
		if err := json.Unmarshal([]byte(call), &entry.ToolCall); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	// Outcomes can arrive after the run checkpoint while their input precedes it.
	seen := map[domain.SessionKey]bool{}
	var preceding []domain.EpisodeEntry
	for _, entry := range entries {
		if entry.InboundKind == domain.InboundEventMessage && entry.Accepted != nil && !*entry.Accepted {
			continue
		}
		if seen[entry.SessionKey] {
			continue
		}
		seen[entry.SessionKey] = true
		var prior domain.EpisodeEntry
		var id int64
		err := r.db.QueryRowContext(ctx, `SELECT id,received_at,sender_id,sender_kind,text FROM events WHERE conversation=? AND received_at<? AND kind='message' AND accepted=1 ORDER BY received_at DESC,id DESC LIMIT 1`, entry.SessionKey, since.UTC()).Scan(&id, &prior.At, &prior.Sender.ID, &prior.Sender.Kind, &prior.Text)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var count int
		var running bool
		err = r.db.QueryRowContext(ctx, `SELECT count(*), (SELECT COALESCE(max(recorded_at),'') FROM model_events WHERE session_key=? AND kind='turn_started' AND recorded_at<?) > (SELECT COALESCE(max(recorded_at),'') FROM model_events WHERE session_key=? AND kind='turn_finished' AND recorded_at<?) FROM events WHERE conversation=? AND kind='message' AND accepted=1 AND received_at<? AND received_at>COALESCE((SELECT max(recorded_at) FROM model_events WHERE session_key=? AND kind='turn_finished' AND recorded_at<?),'')`, entry.SessionKey, since.UTC(), entry.SessionKey, since.UTC(), entry.SessionKey, since.UTC(), entry.SessionKey, since.UTC()).Scan(&count, &running)
		if err != nil {
			return nil, err
		}
		if entry.InboundKind == domain.InboundEventMessage && count == 0 && !running {
			continue
		}
		prior.Ambiguous = count > 1
		accepted := true
		prior.Accepted = &accepted
		prior.ID = fmt.Sprintf("event:%d", id)
		prior.SessionKey = entry.SessionKey
		prior.InboundKind = domain.InboundEventMessage
		preceding = append(preceding, prior)
	}
	entries = append(preceding, entries...)
	slices.SortStableFunc(entries, func(a, b domain.EpisodeEntry) int { return a.At.Compare(b.At) })
	return entries, nil
}
