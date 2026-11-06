package domain

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var ErrNotFound = errors.New("not found")
var ErrRejected = errors.New("input rejected")
var ErrPersistence = errors.New("persistence unavailable")

const TurnDeadlineExceeded = "turn deadline exceeded"

type SentMessage struct {
	ID           string
	Conversation ConversationRef
	Message      OutboundMessage
	SentAt       time.Time
}

type EventRepository interface {
	SaveInbound(ctx context.Context, event InboundEvent) error
	SaveOutbound(ctx context.Context, message SentMessage) error
	ListInbound(ctx context.Context, conversation ConversationRef, limit int) ([]InboundEvent, error)
	ListOutbound(ctx context.Context, conversation ConversationRef, limit int) ([]SentMessage, error)
	Search(ctx context.Context, query string, limit int) ([]InboundEvent, error)
	SaveModel(ctx context.Context, record ModelRecord) error
	ListModel(ctx context.Context, key SessionKey, limit int) ([]ModelRecord, error)
}

type ModelRecord struct {
	SessionKey SessionKey
	Event      ModelEvent
	RecordedAt time.Time
}

type SessionTurn struct {
	Continuations int
	Deadline      time.Time
	Procedure     string
	Failed        bool
}

type Session struct {
	Turn        *SessionTurn
	Key         SessionKey
	ResumeToken string
	Context     *SessionContext
}

type SessionContext struct {
	Conversation ConversationRef
	Direct       bool
	Actor        Actor
	Workdir      string
	TaskName     string
}

type SessionRepository interface {
	Recover(context.Context) error
	Interrupt(context.Context, SessionKey, string) error
	Get(ctx context.Context, key SessionKey) (Session, error)
	Save(ctx context.Context, session Session) error
}

type Schedule struct {
	Actor   Actor
	NextRun time.Time
	Name    string
	Cron    string
	TZ      string
	Prompt  string
	Workdir string
}

type ScheduleRepository interface {
	Save(ctx context.Context, schedule Schedule) error
	List(ctx context.Context) ([]Schedule, error)
	Delete(ctx context.Context, name string) error
}

type OAuthToken struct {
	Registration json.RawMessage
	AccessToken  string
	TokenType    string
	RefreshToken string
	Expiry       time.Time
}

type OAuthTokenRepository interface {
	Get(ctx context.Context, server string) (OAuthToken, error)
	Save(ctx context.Context, server string, token OAuthToken) error
	Refresh(ctx context.Context, server string, previous, token OAuthToken) (bool, error)
}
