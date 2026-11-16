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

// The existing inbox index needs status before it can look up ready_at.
const conversationEvents = `WITH conversation_events AS (
 SELECT e.*, COALESCE(i.session_key,'') AS session_key FROM events e LEFT JOIN inbox i
 ON i.ready_at=e.received_at AND json_extract(i.payload,'$.Event.Conversation')=e.conversation
 AND json_extract(i.payload,'$.Event.Kind')=e.kind AND json_extract(i.payload,'$.Event.MessageRef')=e.message_ref
 AND i.status IN (SELECT DISTINCT status FROM inbox)
) `

func (r *episodeRepository) Since(ctx context.Context, since, until time.Time) ([]domain.EpisodeEntry, error) {
	if !until.After(since) {
		return nil, errors.New("episode window must end after its start")
	}
	rows, err := r.db.QueryContext(ctx, conversationEvents+`
 SELECT source, id, session_key, conversation, at, kind, sender_id, sender_kind, text, tool_call, error, after_send, deleted, accepted FROM (
 SELECT 'event' AS source, e.id, e.session_key, e.conversation, e.received_at AS at, e.kind,
 e.sender_id, e.sender_kind, e.text, 'null' AS tool_call, '' AS error,
 COALESCE((SELECT s.sent_at FROM sends s WHERE s.conversation=e.conversation AND s.sent_at<=e.received_at
 ORDER BY s.sent_at DESC, s.id DESC LIMIT 1), '') >
 COALESCE((SELECT p.received_at FROM events p WHERE p.conversation=e.conversation
 AND (p.received_at,p.id)<(e.received_at,e.id) ORDER BY p.received_at DESC,p.id DESC LIMIT 1), '') AS after_send, e.deleted, e.accepted
 FROM conversation_events e WHERE e.received_at>=? AND e.received_at<?
 UNION ALL
 SELECT 'send', id, '', conversation, sent_at, '', '', '', text, 'null', '', 0, 0, NULL
 FROM sends WHERE sent_at>=? AND sent_at<?
 UNION ALL
 SELECT 'model', id, session_key, '', recorded_at, kind, '', '', text, tool_call, error, 0, 0, NULL
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
		if err := rows.Scan(&source, &id, &entry.SessionKey, &entry.Conversation, &entry.At, &kind, &entry.Sender.ID,
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
	first := map[domain.ConversationRef]domain.EpisodeEntry{}
	var conversations []domain.ConversationRef
	for _, entry := range entries {
		if entry.InboundKind == domain.InboundEventMessage && entry.Accepted != nil && !*entry.Accepted {
			continue
		}
		if _, seen := first[entry.Conversation]; entry.Conversation == "" || seen {
			continue
		}
		first[entry.Conversation] = entry
		conversations = append(conversations, entry.Conversation)
	}
	if len(conversations) == 0 {
		return entries, nil
	}
	refs, err := json.Marshal(conversations)
	if err != nil {
		return nil, err
	}
	priors := map[domain.ConversationRef]domain.EpisodeEntry{}
	priorRows, err := r.db.QueryContext(ctx, conversationEvents+`SELECT id,session_key,conversation,received_at,sender_id,sender_kind,text FROM conversation_events WHERE id IN (
 SELECT (SELECT id FROM events WHERE conversation=j.value AND received_at<? AND kind='message' AND accepted=1 ORDER BY received_at DESC,id DESC LIMIT 1) FROM json_each(?) j
 )`, since.UTC(), string(refs))
	if err != nil {
		return nil, err
	}
	defer priorRows.Close()
	for priorRows.Next() {
		var prior domain.EpisodeEntry
		var id int64
		if err := priorRows.Scan(&id, &prior.SessionKey, &prior.Conversation, &prior.At, &prior.Sender.ID, &prior.Sender.Kind, &prior.Text); err != nil {
			return nil, err
		}
		prior.ID = fmt.Sprintf("event:%d", id)
		priors[prior.Conversation] = prior
	}
	if err := errors.Join(priorRows.Err(), priorRows.Close()); err != nil {
		return nil, err
	}
	var preceding []domain.EpisodeEntry
	for _, conversation := range conversations {
		entry := first[conversation]
		prior, exists := priors[conversation]
		if !exists {
			continue
		}
		var count int
		var running bool
		err = r.db.QueryRowContext(ctx, `SELECT count(*), (SELECT COALESCE(max(recorded_at),'') FROM model_events WHERE session_key=? AND kind='turn_started' AND recorded_at<?) > (SELECT COALESCE(max(recorded_at),'') FROM model_events WHERE session_key=? AND kind='turn_finished' AND recorded_at<?) FROM events WHERE conversation=? AND kind='message' AND accepted=1 AND received_at<? AND received_at>max(COALESCE((SELECT max(recorded_at) FROM model_events WHERE session_key=? AND kind='turn_finished' AND recorded_at<?),''),COALESCE((SELECT max(sent_at) FROM sends WHERE conversation=? AND sent_at<?),''))`, prior.SessionKey, since.UTC(), prior.SessionKey, since.UTC(), entry.Conversation, since.UTC(), prior.SessionKey, since.UTC(), entry.Conversation, since.UTC()).Scan(&count, &running)
		if err != nil {
			return nil, err
		}
		if entry.InboundKind == domain.InboundEventMessage && count == 0 && !running {
			continue
		}
		prior.Ambiguous = count > 1
		accepted := true
		prior.Accepted = &accepted
		prior.InboundKind = domain.InboundEventMessage
		var finished domain.EpisodeEntry
		var id int64
		err = r.db.QueryRowContext(ctx, `SELECT id,recorded_at FROM model_events WHERE session_key=? AND kind='turn_finished' AND recorded_at>=? AND recorded_at<? ORDER BY recorded_at DESC,id DESC LIMIT 1`, prior.SessionKey, prior.At.UTC(), since.UTC()).Scan(&id, &finished.At)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			err = r.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE conversation=? AND kind='message' AND accepted=1 AND received_at<=? AND received_at>max(COALESCE((SELECT max(recorded_at) FROM model_events WHERE session_key=? AND kind='turn_finished' AND recorded_at<?),''),COALESCE((SELECT max(sent_at) FROM sends WHERE conversation=? AND sent_at<?),''))`, entry.Conversation, prior.At.UTC(), prior.SessionKey, prior.At.UTC(), entry.Conversation, prior.At.UTC()).Scan(&count)
			if err != nil {
				return nil, err
			}
			prior.Ambiguous = count > 1
			finished.ID = fmt.Sprintf("model:%d", id)
			finished.SessionKey, finished.ModelKind = prior.SessionKey, domain.ModelEventTurnFinished
			preceding = append(preceding, finished)
		}
		preceding = append(preceding, prior)
		var reply domain.EpisodeEntry
		err = r.db.QueryRowContext(ctx, `SELECT id,sent_at,text FROM sends WHERE conversation=? AND sent_at>=? AND sent_at<? ORDER BY sent_at DESC,id DESC LIMIT 1`, entry.Conversation, prior.At.UTC(), since.UTC()).Scan(&id, &reply.At, &reply.Text)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		reply.ID = fmt.Sprintf("send:%d", id)
		reply.Conversation, reply.Sent = entry.Conversation, true
		preceding = append(preceding, reply)
	}
	entries = append(preceding, entries...)
	slices.SortStableFunc(entries, func(a, b domain.EpisodeEntry) int { return a.At.Compare(b.At) })
	return entries, nil
}
