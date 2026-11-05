package domain

import (
	"context"
	"encoding/json"
	"regexp"
)

type Actor string

const (
	ActorOwner  Actor = "owner"
	ActorMember Actor = "member"
)

type Tool struct {
	ReadOnly    bool
	Name        string
	Description string
	InputSchema json.RawMessage
	OwnerOnly   bool
	TextResult  bool
	Handler     func(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}

type ToolSession struct {
	Key   SessionKey
	Actor Actor
}

type toolSessionKey struct{}

func WithToolSession(ctx context.Context, session ToolSession) context.Context {
	return context.WithValue(ctx, toolSessionKey{}, session)
}

func ToolSessionFromContext(ctx context.Context) (ToolSession, bool) {
	session, ok := ctx.Value(toolSessionKey{}).(ToolSession)
	return session, ok
}

type ToolHub interface {
	Start(ctx context.Context) error
	Shutdown(ctx context.Context) error
	AddSource(name string, tools []Tool) error
	ReplaceSource(name string, tools []Tool) error
	OpenSession(key SessionKey, actor Actor) (MCPEndpoint, error)
	CloseSession(key SessionKey)
}

type ExternalTools interface {
	Shutdown(context.Context) error
	Connect(ctx context.Context) error
	Start(ctx context.Context) error
}

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func ValidToolName(name string) bool { return toolNamePattern.MatchString(name) }
