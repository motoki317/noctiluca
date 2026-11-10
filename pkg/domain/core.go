package domain

import "context"

type AcceptRule struct {
	Users          []string
	Groups         []string
	Bots           []string
	Ignore         bool
	RequireMention bool
}

// PromptAssembler loads instructions when a model process opens, including on resume.
type PromptAssembler func(context.Context, ToolSession) (string, error)

type Core interface {
	Start(context.Context) error
	Shutdown(context.Context) error
}
