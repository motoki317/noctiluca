package domain

import "context"

type AcceptRule struct {
	Users          []string
	Groups         []string
	AllowBots      bool
	RequireMention bool
}

// PromptAssembler refreshes context before each input, including inputs sent during a turn.
type PromptAssembler func(context.Context, ToolSession, UserInput) (UserInput, error)

type Core interface {
	Start(context.Context) error
	Shutdown(context.Context) error
	UpdateUserGroups(context.Context, map[string][]string) error
}
